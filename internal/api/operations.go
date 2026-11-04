package api

import (
	"errors"
	"os"
	"strings"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// operations exposes explicit operator folder registration. It never copies or deletes models.
func (s *Server) operations(q *request, path, method string) (bool, error) {
	if path != "/v1/folders" && !strings.HasPrefix(path, "/v1/folders/") {
		return false, nil
	}
	if err := q.requireOperator(); err != nil {
		return true, err
	}
	folders := s.Runtime.Folders
	switch {
	case path == "/v1/folders" && method == "GET":
		return true, q.send(200, jsonv.From(folders.List()))
	case path == "/v1/folders" && method == "POST":
		body, err := q.body()
		if err != nil {
			return true, err
		}
		parsed, err := c.Validate(c.StrictObject(c.Field{Name: "path", Check: c.Str(c.StrOpts{Min: 1})}), body)
		if err != nil {
			return true, err
		}
		folder, err := folders.Add(parsed.Str("path"))
		if err != nil {
			return true, c.Err(400, "invalid_folder", err.Error(), "path")
		}
		if err = s.Runtime.RefreshFolders(); err != nil {
			return true, err
		}
		return true, q.send(201, jsonv.From(folder))
	case strings.HasPrefix(path, "/v1/folders/") && method == "DELETE":
		id := strings.TrimPrefix(path, "/v1/folders/")
		if strings.Contains(id, "/") {
			return true, c.Err(404, "not_found", "Folder not found")
		}
		err := folders.Remove(id)
		if errors.Is(err, os.ErrNotExist) {
			return true, c.Err(404, "not_found", "Folder not found")
		}
		if err != nil {
			return true, err
		}
		return true, q.send(200, jsonv.O("removed", true, "files_deleted", false))
	}
	return true, c.Err(404, "not_found", "Endpoint not found")
}
