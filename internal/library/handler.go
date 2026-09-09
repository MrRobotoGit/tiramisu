package library

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxBodyBytes = 1 << 20

// Handler exposes the manager over HTTP: POST /api/library/add,
// POST /api/library/remove and GET /api/library/list. It exists so a client with no
// access to the filesystem can still file a title into the library.
type Handler struct {
	mgr *Manager
}

func NewHandler(m *Manager) *Handler { return &Handler{mgr: m} }

func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req AddRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Add(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status := http.StatusCreated
	if resp.AlreadyPresent {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req RemoveRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Remove(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	items, err := h.mgr.List(r.URL.Query().Get("type"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func decode(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeAPIError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.Status, apiErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
