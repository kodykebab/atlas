package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

var errNoSuchImage = errors.New("no such image")

func usable(image atlas.Image) bool {
	return image.Enabled && image.Status == "available"
}

func (server *Server) listImages(writer http.ResponseWriter, request *http.Request, caller atlas.Caller) {
	images, err := server.config.Atlas.Images(request.Context(), caller)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}

	items := []map[string]any{}
	for _, image := range images {
		if !usable(image) {
			continue
		}
		items = append(items, map[string]any{
			"Id":          image.ID,
			"ParentId":    "",
			"RepoTags":    []string{image.Title},
			"RepoDigests": []string{},
			"Created":     image.CreatedAt,
			"Size":        int64(image.RootfsSizeMiB) << 20,
			"SharedSize":  -1,
			"Labels":      map[string]string{},
			"Containers":  -1,
		})
	}
	writeJSON(writer, http.StatusOK, items)
}

func (server *Server) resolveImage(ctx context.Context, caller atlas.Caller, reference string) (atlas.Image, error) {
	images, err := server.config.Atlas.Images(ctx, caller)
	if err != nil {
		return atlas.Image{}, err
	}

	title := strings.TrimSuffix(reference, ":latest")
	var matches []atlas.Image
	for _, image := range images {
		if !usable(image) {
			continue
		}
		if image.ID == reference {
			return image, nil
		}
		if image.Title == reference || image.Title == title {
			matches = append(matches, image)
		}
	}
	switch len(matches) {
	case 0:
		return atlas.Image{}, errNoSuchImage
	case 1:
		return matches[0], nil
	default:
		return atlas.Image{}, &atlas.Error{Status: http.StatusConflict, Message: fmt.Sprintf("%d Atlas images are titled %q; use the image ID from docker images", len(matches), reference)}
	}
}
