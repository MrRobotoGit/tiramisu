package friends

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

const maxBody = 4 << 10

type apiErr struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { writeJSON(w, code, apiErr{msg}) }

// failFor maps a Manager error to a stable status and error code.
func failFor(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInviteExpired):
		fail(w, http.StatusConflict, "expired")
	case errors.Is(err, ErrOwnInvite):
		fail(w, http.StatusConflict, "own_invite")
	case errors.Is(err, ErrAlreadyDecided):
		fail(w, http.StatusConflict, "already_decided")
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrBadName):
		fail(w, http.StatusBadRequest, "bad_name")
	case errors.Is(err, ErrNoPublicAddr):
		fail(w, http.StatusServiceUnavailable, "public_address_unknown")
	case errors.Is(err, ErrUnreachable):
		fail(w, http.StatusBadGateway, "unreachable")
	case errors.Is(err, ErrHasMaster):
		fail(w, http.StatusConflict, "has_master")
	case errors.Is(err, ErrHasSlaves):
		fail(w, http.StatusConflict, "has_slaves")
	case errors.Is(err, ErrLibraryRole):
		fail(w, http.StatusConflict, "library_role")
	case errors.Is(err, ErrOfferGone):
		fail(w, http.StatusConflict, "offer_gone")
	case errors.Is(err, ErrNoLibrary):
		fail(w, http.StatusServiceUnavailable, "library_disabled")
	default: // a store error or a corrupt row (ErrUnexpected)
		fail(w, http.StatusInternalServerError, "store")
	}
}

// decodeBody reads one JSON object, rejecting unknown fields and trailing data.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad_json")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		fail(w, http.StatusBadRequest, "bad_json")
		return false
	}
	return true
}

type selfView struct {
	Name  string    `json:"name"`
	Addr  string    `json:"addr,omitempty"`
	Reach reachView `json:"reach"`
}

type reachView struct {
	State    string     `json:"state"` // checking | reachable
	Last     *time.Time `json:"last,omitempty"`
	Networks []string   `json:"networks"`
}

type friendView struct {
	Name     string         `json:"name"`
	State    string         `json:"state"`
	Role     string         `json:"role"`
	Addr     string         `json:"addr,omitempty"`
	LastSeen int64          `json:"last_seen,omitempty"`
	Since    int64          `json:"since,omitempty"`
	Now      *Activity      `json:"now,omitempty"` // what the friend and this node exchange right now
	Library  *friendLibView `json:"library,omitempty"`
	Online   *bool          `json:"online,omitempty"`  // answered in the last period; absent before the first check
	Checked  int64          `json:"checked,omitempty"` // unix time of that verdict
	Secure   bool           `json:"secure"`            // seen on the encrypted control channel: plaintext refused
}

type requestView struct {
	ID        int64  `json:"id"`
	Direction string `json:"direction"` // in: they used our code; out: we used theirs
	Name      string `json:"declared_name"`
	State     string `json:"state"`
	Expires   int64  `json:"expires"`
	Created   int64  `json:"created"`
}

type inviteView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Expires int64  `json:"expires"`
}

type listView struct {
	Self     selfView      `json:"self"`
	Friends  []friendView  `json:"friends"`
	Requests []requestView `json:"requests"`
	Invites  []inviteView  `json:"invites"`
	Library  *libraryView  `json:"library,omitempty"`
}

// view builds the GET /api/friends answer; it carries no key, secret or invite code.
func (m *Manager) view() (listView, error) {
	out := listView{Friends: []friendView{}, Requests: []requestView{}, Invites: []inviteView{}}
	fs, err := m.store.ListFriends()
	if err != nil {
		return out, err
	}
	invs, err := m.store.ListInvites()
	if err != nil {
		return out, err
	}
	reqs, err := m.store.ListRequests()
	if err != nil {
		return out, err
	}
	now := m.now().Unix()
	var act map[string]*Activity
	if f := m.activity.Load(); f != nil {
		act = (*f)()
	}
	libs, root := m.libraryViews()
	out.Library = root
	m.mu.Lock()
	for _, f := range fs {
		seen := f.LastSeen
		if a := m.seen[f.Name]; a != nil && a.Load() > seen {
			seen = a.Load()
		}
		v := friendView{Name: f.Name, State: "paired", Role: f.Role, Addr: f.Addr, LastSeen: seen,
			Since: f.CreatedAt, Now: act[f.Name], Library: libs[f.Name], Secure: f.Secure}
		if s := m.secure[f.Name]; s != nil && s.Load() != f.Secure {
			v.Secure = s.Load() // set since the read above
		}
		if len(f.PairKey) == len(PairKey{}) {
			if on, at, ok := m.presence(ControlHash(PairKey(f.PairKey))); ok {
				v.Online, v.Checked = &on, at
			}
		}
		out.Friends = append(out.Friends, v)
	}
	m.mu.Unlock()
	for _, r := range reqs {
		if r.ExpiresAt > now {
			out.Requests = append(out.Requests, requestView{ID: r.ID, Direction: r.Direction, Name: r.Name, State: r.State, Expires: r.ExpiresAt, Created: r.CreatedAt})
		}
	}
	for _, in := range invs {
		if in.ExpiresAt > now && (in.State == "open" || in.State == "requested") {
			out.Invites = append(out.Invites, inviteView{ID: hex.EncodeToString(in.Hash), State: in.State, Expires: in.ExpiresAt})
		}
	}
	out.Self.Name = m.SelfName()
	if ap := m.lastIP.Load(); ap != nil {
		out.Self.Addr = ap.String()
	}
	rc := m.Reach()
	out.Self.Reach = reachView{State: "checking", Networks: rc.Networks}
	if rc.Reachable {
		out.Self.Reach.State = "reachable"
	}
	if !rc.Last.IsZero() {
		out.Self.Reach.Last = &rc.Last
	}
	return out, nil
}

// Register mounts /api/friends on mux (503 while get returns nil). Reads are open to API tokens;
// actions need allowed(r), a browser session when sign-in is on.
func Register(mux *http.ServeMux, get func() *Manager, allowed func(*http.Request) bool) {
	read := func(h func(http.ResponseWriter, *http.Request, *Manager)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			m := get()
			if m == nil {
				fail(w, http.StatusServiceUnavailable, "friends_disabled")
				return
			}
			h(w, r, m)
		}
	}
	act := func(h func(http.ResponseWriter, *http.Request, *Manager)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if allowed != nil && !allowed(r) {
				fail(w, http.StatusForbidden, "session_required")
				return
			}
			read(h)(w, r)
		}
	}
	type codeReq struct {
		Code string `json:"code"`
	}
	mux.HandleFunc("GET /api/friends", read(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		v, err := m.view()
		if err != nil {
			fail(w, http.StatusInternalServerError, "store")
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.HandleFunc("POST /api/friends/invite", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		inv, err := m.CreateInvite(r.Context())
		if err != nil {
			failFor(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": inv.ID, "code": inv.Code, "expires": inv.Expires.Unix()})
	}))
	mux.HandleFunc("DELETE /api/friends/invites/{id}", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		if err := m.CancelInvite(r.PathValue("id")); err != nil {
			failFor(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/friends/preview", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		var req codeReq
		if !decodeBody(w, r, &req) {
			return
		}
		inv, err := DecodeCode(req.Code)
		if err != nil {
			fail(w, http.StatusBadRequest, "bad_code")
			return
		}
		p, err := m.Preview(r.Context(), req.Code)
		if err != nil {
			failFor(w, err)
			return
		}
		// The name is what the other node declares; the hello is not authenticated.
		writeJSON(w, http.StatusOK, map[string]any{"declared_name": p.Name, "expires": p.Expires.Unix(), "addr": inv.Addr.String()})
	}))
	mux.HandleFunc("POST /api/friends/join", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		var req codeReq
		if !decodeBody(w, r, &req) {
			return
		}
		if _, err := DecodeCode(req.Code); err != nil {
			fail(w, http.StatusBadRequest, "bad_code")
			return
		}
		id, err := m.Join(r.Context(), req.Code)
		switch {
		case errors.Is(err, ErrNoAnswer):
			writeJSON(w, http.StatusAccepted, map[string]any{"request_id": id, "pending": true})
		case err != nil:
			failFor(w, err)
		default:
			writeJSON(w, http.StatusAccepted, map[string]any{"request_id": id})
		}
	}))
	decide := func(approve bool) http.HandlerFunc {
		return act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 {
				fail(w, http.StatusBadRequest, "bad_id")
				return
			}
			if approve {
				err = m.ApproveAsync(id)
			} else {
				err = m.DeclineAsync(id)
			}
			if err != nil {
				failFor(w, err)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
	}
	mux.HandleFunc("POST /api/friends/requests/{id}/approve", decide(true))
	mux.HandleFunc("POST /api/friends/requests/{id}/decline", decide(false))
	mux.HandleFunc("DELETE /api/friends/{name}", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		if err := m.Remove(r.Context(), r.PathValue("name")); err != nil {
			failFor(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	// /library/{name}, not /{name}/library: that pattern would collide with /invites/{id}.
	library := func(do func(*Manager, *http.Request, string) error, ok int) http.HandlerFunc {
		return act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
			if err := do(m, r, r.PathValue("name")); err != nil {
				failFor(w, err)
				return
			}
			w.WriteHeader(ok)
		})
	}
	mux.HandleFunc("POST /api/friends/library/{name}", library(func(m *Manager, r *http.Request, name string) error {
		return m.OfferLibrary(r.Context(), name)
	}, http.StatusAccepted))
	mux.HandleFunc("POST /api/friends/library/{name}/accept", library(func(m *Manager, r *http.Request, name string) error {
		return m.AcceptLibrary(r.Context(), name)
	}, http.StatusAccepted))
	mux.HandleFunc("DELETE /api/friends/library/{name}", library(func(m *Manager, r *http.Request, name string) error {
		return m.EndLibrary(r.Context(), name)
	}, http.StatusNoContent))
	mux.HandleFunc("POST /api/friends/reset-encryption/{name}", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		if err := m.ResetEncryption(r.PathValue("name")); err != nil {
			failFor(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("PUT /api/friends/self", act(func(w http.ResponseWriter, r *http.Request, m *Manager) {
		var req struct {
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if err := m.SetSelfName(req.Name); err != nil {
			failFor(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
