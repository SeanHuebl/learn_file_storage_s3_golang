package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to parse file", err)
		return
	}
	defer file.Close()
	mediaType := header.Header.Get("Content-type")
	data, err := io.ReadAll(file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to read media data", err)
		return
	}
	metaData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to locate video", err)
		return
	}
	thumbnail := thumbnail{
		data:      data,
		mediaType: mediaType,
	}
	videoThumbnails[videoID] = thumbnail

	thumbnailURL := fmt.Sprintf("localhost:%v/api/thumbnails/%v", cfg.port, videoID)
	metaData.ThumbnailURL = &thumbnailURL

	err = cfg.db.UpdateVideo(metaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to update metadata", err)
		return
	}
	imgDataString := base64.StdEncoding.EncodeToString(thumbnail.data)
	dataURL := fmt.Sprintf("data:%v;base64,%v", thumbnail.mediaType, imgDataString)

	respondWithJSON(w, http.StatusOK, metaData)
}
