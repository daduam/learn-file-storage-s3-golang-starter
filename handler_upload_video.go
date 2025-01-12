package main

import (
	"bytes"
	"context"
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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil || video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mimeType := header.Header.Get("content-type")
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Upload a valid mp4 video", err)
		return
	}

	extensions, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(extensions) == 0 {
		respondWithError(w, http.StatusBadRequest, "Upload a valid mp4 video", err)
		return
	}

	tempfile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	defer os.Remove(tempfile.Name())
	defer tempfile.Close()

	if _, err = io.Copy(tempfile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	if _, err = tempfile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tempfile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video aspect ratio", err)
		return
	}

	var keyPrefix string
	if aspectRatio == "16:9" {
		keyPrefix = "landscape"
	} else if aspectRatio == "9:16" {
		keyPrefix = "portrait"
	} else {
		keyPrefix = "other"
	}

	// process for faststart
	processedFilePath, err := processVideoForFastStart(tempfile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video for faststart", err)
		return
	}

	processedTempFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer os.Remove(processedTempFile.Name())
	defer processedTempFile.Close()

	b := make([]byte, 32)
	_, err = rand.Read(b)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}
	filenameNoExt := base64.RawURLEncoding.EncodeToString(b)

	objectKey := fmt.Sprintf("%s/%s", keyPrefix, filenameNoExt+extensions[0])

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(objectKey),
		Body:        processedTempFile,
		ContentType: aws.String(mediaType),
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, objectKey)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error mapping db video to signed video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

type FFProbeShowStreamsReponse struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	var out bytes.Buffer

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %v", err)
	}

	var streams FFProbeShowStreamsReponse
	err := json.Unmarshal(out.Bytes(), &streams)
	if err != nil {
		return "", err
	}

	if len(streams.Streams) == 0 {
		return "", fmt.Errorf("no streams found in video file")
	}

	ratio := float64(streams.Streams[0].Width) / float64(streams.Streams[0].Height)

	if ratio >= 1.7 && ratio <= 1.8 {
		return "16:9", nil
	}

	if ratio >= 0.5 && ratio <= 0.6 {
		return "9:16", nil
	}

	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outFilePath)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffmpeg: %v", err)
	}

	return outFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	req, err := presignClient.PresignGetObject(context.TODO(),
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		},
		func(po *s3.PresignOptions) {
			po.Expires = expireTime
		},
	)
	if err != nil {
		return "", fmt.Errorf("error generating presigned url: %v", err)
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return database.Video{}, errors.New("error mapping db video to video with presigned url")
	}

	videoURL, err := generatePresignedURL(cfg.s3Client, parts[0], parts[1], 30*time.Minute)
	if err != nil {
		return database.Video{}, fmt.Errorf("error mapping db video to video with presigned url: %v", err)
	}

	video.VideoURL = &videoURL

	return video, nil
}
