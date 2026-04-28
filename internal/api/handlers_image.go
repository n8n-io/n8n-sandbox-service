package api

import (
	"errors"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/manager"
	"github.com/n8n-io/sandbox-service/internal/store"
)

// ImageResponse is the JSON body for image list/get responses.
type ImageResponse struct {
	ID            string `json:"id"`
	Tag           string `json:"tag"`
	BaseImage     string `json:"base_image"`
	DockerImageID string `json:"docker_image_id"`
	CreatedAt     int64  `json:"created_at"`
}

func imageResponseFrom(rec *store.ImageRecord) *ImageResponse {
	return &ImageResponse{
		ID:            rec.ID,
		Tag:           rec.Tag,
		BaseImage:     rec.BaseImage,
		DockerImageID: rec.DockerImageID,
		CreatedAt:     rec.CreatedAt,
	}
}

// ListImages handles GET /images.
func ListImages(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, err := mgr.ListImages(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]*ImageResponse, len(records))
		for i, rec := range records {
			resp[i] = imageResponseFrom(rec)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// GetImage handles GET /images/{id}.
func GetImage(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}

		rec, err := mgr.GetImage(r.Context(), id)
		if err != nil {
			if errors.Is(err, manager.ErrImageNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, imageResponseFrom(rec))
	}
}

// DeleteImage handles DELETE /images/{id}.
func DeleteImage(mgr SandboxManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}
		if err := mgr.DeleteImage(r.Context(), id); err != nil {
			switch {
			case errors.Is(err, manager.ErrImageNotFound):
				writeError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, manager.ErrImageInUse):
				writeError(w, http.StatusConflict, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
