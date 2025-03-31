package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
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

	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for thumbnail", nil)
		return
	}

	mimeType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Invalid media type", err)
		return
	}
	if mimeType != "video/mp4" {
		respondWithError(w, http.StatusInternalServerError, "Must be an mp4", err)
		return
	}

	tmp, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	fileExtension := strings.Split(mimeType, "/")[1]
	name := make([]byte, 32)
	rand.Read(name)
	fileName := base64.RawURLEncoding.EncodeToString(name) + "." + fileExtension

	_, err = io.Copy(tmp, file)
	if err != nil {
		fmt.Println(err)
		return
	}

	processedVideo, err := processVideoForFastStart(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}

	ratio, err := getVideoAspectRatio(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}

	switch ratio {
	case "16:9":
		fileName = "landscape/" + fileName
	case "9:16":
		fileName = "portrait/" + fileName
	default:
		fileName = "other/" + fileName
	}

	processedFile, err := os.ReadFile(processedVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't read processed file", err)
		return
	}

	video_url := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, fileName)

	tmp.Seek(0, io.SeekStart)
	params := s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &fileName, Body: bytes.NewReader(processedFile), ContentType: &mimeType}
	cfg.s3Client.PutObject(r.Context(), &params)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}
	fmt.Println(video_url)
	video.VideoURL = &video_url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	type AspectRatio struct {
		Streams []struct {
			DisplayAspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}

	probe := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := bytes.Buffer{}
	probe.Stdout = &buffer
	err := probe.Run()
	if err != nil {
		return "", errors.New("ffprobe failed")
	}

	ratio := AspectRatio{}
	err = json.Unmarshal(buffer.Bytes(), &ratio)
	if err != nil {
		fmt.Println("JSON Unmarshal Error:", err)
		fmt.Println("ffprobe output:", buffer.String())
		return "", errors.New("invalid JSON from ffprobe")
	}

	if len(ratio.Streams) == 0 {
		return "", errors.New("no streams found")
	}

	aspectRatio := ratio.Streams[0].DisplayAspectRatio

	return aspectRatio, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	newPath := filePath + ".processing"
	processing := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newPath)

	err := processing.Run()
	if err != nil {
		return "", err
	}

	return newPath, nil
}
