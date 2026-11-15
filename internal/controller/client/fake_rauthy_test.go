package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fluencelabs/rauthy-go"
)

const fakeSecret = "s3cr3t-value"

// fakeRauthy is an in-memory stand-in for the Rauthy client endpoints. Like
// the real server, POST /clients keeps only the fields of NewClientRequest and
// fills the rest with defaults; PUT replaces the whole client.
type fakeRauthy struct {
	mu      sync.Mutex
	clients map[string]*rauthy.ClientResponse
	calls   []string
	// failPut makes every PUT answer 400.
	failPut bool
	// failPost makes every POST answer 500 without creating anything.
	failPost bool
	// failDelete makes every DELETE answer 500.
	failDelete bool
	// commitThenFail makes POST create the client and still answer 500, as
	// when the response is lost.
	commitThenFail bool
	// raceOnPost, when set, is stored under the requested id just before the
	// next POST is handled: a third party taking the id first.
	raceOnPost *rauthy.ClientResponse
	authz      string
	srv        *httptest.Server
}

func newFakeRauthy(t *testing.T) *fakeRauthy {
	t.Helper()
	f := &fakeRauthy{clients: map[string]*rauthy.ClientResponse{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRauthy) api(t *testing.T) *rauthy.Client {
	t.Helper()
	c, err := rauthy.New(f.srv.URL, "test$key", rauthy.WithRetryPolicy(1, 0, 0))
	if err != nil {
		t.Fatalf("rauthy.New: %v", err)
	}
	return c
}

// put stores a client directly, as if somebody had created it in Rauthy.
func (f *fakeRauthy) put(c *rauthy.ClientResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *c
	f.clients[c.ID] = &cp
}

// stored returns the test client as Rauthy holds it, or nil.
func (f *fakeRauthy) stored() *rauthy.ClientResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[testID]
	if !ok {
		return nil
	}
	cp := *c
	return &cp
}

func (f *fakeRauthy) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRauthy) deleteStored() {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.clients, testID)
}

func (f *fakeRauthy) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeRauthy) lastAuthorization() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authz
}

func (f *fakeRauthy) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path, ok := strings.CutPrefix(r.URL.Path, rauthy.APIBasePath)
	if !ok {
		// Not the API: something on the host probing the test port. Neither
		// recorded nor answered as Rauthy.
		writeError(w, http.StatusNotFound, "not the Rauthy API")
		return
	}
	f.calls = append(f.calls, r.Method+" "+path)
	f.authz = r.Header.Get("Authorization")

	rest, isClients := strings.CutPrefix(path, "/clients")
	switch {
	case !isClients:
		writeError(w, http.StatusNotFound, "no such route")
	case rest == "" && r.Method == http.MethodPost:
		f.create(w, r)
	case strings.HasSuffix(rest, "/secret") && r.Method == http.MethodPost:
		f.secret(w, strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/secret"))
	case strings.HasPrefix(rest, "/"):
		f.byID(w, r, strings.TrimPrefix(rest, "/"))
	default:
		writeError(w, http.StatusMethodNotAllowed, "unsupported")
	}
}

func (f *fakeRauthy) create(w http.ResponseWriter, r *http.Request) {
	if f.failPost {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	var req rauthy.NewClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.raceOnPost != nil {
		cp := *f.raceOnPost
		cp.ID = req.ID
		f.clients[req.ID] = &cp
		f.raceOnPost = nil
	}
	if _, exists := f.clients[req.ID]; exists {
		writeError(w, http.StatusBadRequest, "client id already exists")
		return
	}
	c := &rauthy.ClientResponse{
		ID:                     req.ID,
		Name:                   req.Name,
		Enabled:                true,
		Confidential:           req.Confidential,
		RedirectURIs:           req.RedirectURIs,
		PostLogoutRedirectURIs: req.PostLogoutRedirectURIs,
		FlowsEnabled:           []string{"authorization_code"},
		AccessTokenAlg:         "EdDSA",
		IDTokenAlg:             "EdDSA",
		AuthCodeLifetime:       60,
		AccessTokenLifetime:    1800,
		Scopes:                 []string{"openid", "email", "profile", "groups"},
		DefaultScopes:          []string{"openid"},
	}
	if !req.Confidential {
		c.Challenges = []string{"S256"}
	}
	f.clients[req.ID] = c
	if f.commitThenFail {
		writeError(w, http.StatusInternalServerError, "connection reset")
		return
	}
	writeJSON(w, c)
}

func (f *fakeRauthy) byID(w http.ResponseWriter, r *http.Request, id string) {
	c, exists := f.clients[id]
	if !exists {
		writeError(w, http.StatusNotFound, "client not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, c)
	case http.MethodPut:
		f.update(w, r, id)
	case http.MethodDelete:
		if f.failDelete {
			writeError(w, http.StatusInternalServerError, "database unavailable")
			return
		}
		delete(f.clients, id)
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, http.StatusMethodNotAllowed, "unsupported")
	}
}

func (f *fakeRauthy) update(w http.ResponseWriter, r *http.Request, id string) {
	if f.failPut {
		writeError(w, http.StatusBadRequest, "rejected")
		return
	}
	var req rauthy.UpdateClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Rauthy refuses a PUT without these; an empty value means the caller
	// forgot to fill an unset field from the observed client.
	if len(req.FlowsEnabled) == 0 || req.AccessTokenAlg == "" || req.AuthCodeLifetime == 0 {
		writeError(w, http.StatusBadRequest, "missing required field")
		return
	}
	c := &rauthy.ClientResponse{
		ID:                     id,
		Name:                   req.Name,
		Enabled:                req.Enabled,
		Confidential:           req.Confidential,
		RedirectURIs:           req.RedirectURIs,
		PostLogoutRedirectURIs: req.PostLogoutRedirectURIs,
		AllowedOrigins:         req.AllowedOrigins,
		FlowsEnabled:           req.FlowsEnabled,
		AccessTokenAlg:         req.AccessTokenAlg,
		IDTokenAlg:             req.IDTokenAlg,
		AuthCodeLifetime:       req.AuthCodeLifetime,
		AccessTokenLifetime:    req.AccessTokenLifetime,
		Scopes:                 req.Scopes,
		DefaultScopes:          req.DefaultScopes,
		Challenges:             req.Challenges,
		ForceMFA:               req.ForceMFA,
		ClientURI:              req.ClientURI,
		Contacts:               req.Contacts,
		BackchannelLogoutURI:   req.BackchannelLogoutURI,
		RestrictGroupPrefix:    req.RestrictGroupPrefix,
		Scim:                   req.Scim,
	}
	f.clients[id] = c
	writeJSON(w, c)
}

func (f *fakeRauthy) secret(w http.ResponseWriter, id string) {
	c, exists := f.clients[id]
	if !exists {
		writeError(w, http.StatusNotFound, "client not found")
		return
	}
	resp := rauthy.ClientSecretResponse{ID: id, Confidential: c.Confidential}
	if c.Confidential {
		s := fakeSecret
		resp.Secret = &s
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status), "message": msg})
}
