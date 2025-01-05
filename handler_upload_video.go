package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

type video struct {
	Streams []struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"streams"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadLimit = 1 << 30

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}
	metaData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error finding metadata", err)
		return
	}
	if metaData.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "access denied", err)
		return
	}

	fmt.Println("uploading footage for video", videoID, "by user", userID)
	r.ParseMultipartForm(maxUploadLimit)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to form file", err)
		return
	}
	defer file.Close()
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "file must be of type .mp4", err)
		return
	}

	// save the upload to temp file on disk
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to copy file", err)
		return
	}
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to process video for fast start", err)
		return
	}
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to open processed video", err)
		return
	}
	defer processedFile.Close()

	ratio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to get aspect ratio", err)
		return
	}
	var layout string

	switch ratio {
	case "16:9":
		layout = "landscape"
	case "9:16":
		layout = "portrait"
	default:
		layout = "other"
	}

	processedFile.Seek(0, io.SeekStart)
	fileExtension, _ := strings.CutPrefix(mediaType, "video/")
	rnd32 := make([]byte, 32)
	_, err = rand.Read(rnd32)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to generate random bytes", err)
		return
	}

	fileID := base64.RawURLEncoding.EncodeToString(rnd32)
	fileName := fmt.Sprintf("%v/%v.%v", layout, fileID, fileExtension)

	params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileName,
		Body:        processedFile,
		ContentType: &mediaType,
	}

	_, err = cfg.s3Client.PutObject(context.Background(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to put object in s3", err)
		return
	}
	fmt.Printf("video uploaded to s3")
	videoURL := fmt.Sprintf("%v,%v", cfg.s3Bucket, fileName)
	metaData.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(metaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to update metadata", err)
		return
	}
	respondWithJSON(w, http.StatusOK, metaData)
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer
	cmd.Run()
	var video video
	err := json.Unmarshal(buffer.Bytes(), &video)
	if err != nil {
		return "", fmt.Errorf("unable to marshal data: %v", err)
	}

	if len(video.Streams) == 0 {
		return "", fmt.Errorf("no streams available")
	}
	v := video.Streams[0]
	if v.Width == 0 || v.Height == 0 {
		return "", fmt.Errorf("resolution cannot be 0")
	}

	ratio := floatToThreeDecimals(v.Width / v.Height)

	switch ratio {
	case floatToThreeDecimals(16.0 / 9.0):
		return "16:9", nil
	case floatToThreeDecimals(9.0 / 16.0):
		return "9:16", nil
	default:
		return "other", nil
	}
}

func floatToThreeDecimals(float float64) float64 {
	return math.Floor(float*1000) / 1000
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("unable to run command")
	}
	return outputFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	params := s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	presignObj, err := presignClient.PresignGetObject(context.Background(), &params, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("unable to get presigned object: %v", err)
	}
	return presignObj.URL, nil
}
func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	params := strings.Split(*video.VideoURL, ",")
	bucket := params[0]
	key := params[1]
	preSignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Minute*15)
	if err != nil {
		return database.Video{}, fmt.Errorf("error generating presigned url")
	}
	video.VideoURL = &preSignedURL
	return video, nil
}
