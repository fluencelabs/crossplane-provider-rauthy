package client_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/fluencelabs/rauthy-go"
)

const fakeSecret = "s3cr3t-value"

// fakeRauthy is an in-memory stand-in for the Rauthy client endpoints. Like
// the real server, POST /clients keeps only the fields of NewClientRequest and
// fills the rest with defaults; PUT replaces the whole client.
type fakeRauthy struct {
	mu      sync.Mutex
	clients map[string]*rauthy.ClientResponse
	// secrets holds the secret of each confidential client. Clients created
	// through POST get a fresh one; clients stored with put get fakeSecret.
	secrets map[string]string
	created int
	// failSecret makes every secret read answer 500.
	failSecret bool
	calls      []string
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
	// redirectTo, when set, answers every API request with a 308 to it, as a
	// proxy in front of Rauthy might.
	redirectTo string
	// lastPut is the body of the last PUT /clients/{id}, and lastPutRaw the
	// same body as sent.
	lastPut    *rauthy.UpdateClientRequest
	lastPutRaw []byte
	authz      string
	srv        *httptest.Server
}

func newFakeRauthy(t *testing.T) *fakeRauthy {
	t.Helper()
	f := &fakeRauthy{clients: map[string]*rauthy.ClientResponse{}, secrets: map[string]string{}}
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
	f.putWithSecret(c, fakeSecret)
}

// putWithSecret stores a client with the given secret, as if somebody had
// created it in Rauthy.
func (f *fakeRauthy) putWithSecret(c *rauthy.ClientResponse, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *c
	f.clients[c.ID] = &cp
	f.secrets[c.ID] = secret
}

// secretOf returns the test client's current secret.
func (f *fakeRauthy) secretOf() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secrets[testID]
}

// rotate gives the test client a new secret, as the Rauthy UI does.
func (f *fakeRauthy) rotate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[testID] = "rotated-secret"
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
	delete(f.secrets, testID)
}

func (f *fakeRauthy) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// lastPutBody returns the body of the last PUT, or nil.
func (f *fakeRauthy) lastPutBody() *rauthy.UpdateClientRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPut
}

// lastPutField returns one field of the last PUT body exactly as sent, or nil
// when the body did not carry it.
func (f *fakeRauthy) lastPutField(t *testing.T, name string) json.RawMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastPutRaw == nil {
		t.Fatal("no PUT was sent")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(f.lastPutRaw, &body); err != nil {
		t.Fatalf("parse the PUT body: %v", err)
	}
	return body[name]
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
		http.NotFound(w, r)
		return
	}
	f.calls = append(f.calls, r.Method+" "+path)
	f.authz = r.Header.Get("Authorization")
	if f.redirectTo != "" {
		http.Redirect(w, r, f.redirectTo+r.URL.Path, http.StatusPermanentRedirect)
		return
	}

	rest, isClients := strings.CutPrefix(path, "/clients")
	switch {
	case !isClients:
		http.NotFound(w, r)
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

// Rauthy v0.36.2 and v0.37.2 src/common/src/regex.rs, RE_CLIENT_NAME.
func validClientName(name *string) bool {
	clientNamePatterns := []*regexp.Regexp{
		regexp.MustCompile(
			`^[a-zA-Z0-9À-ɏ()\-\s\x{000B}\x{0085}\p{Z}\x{3041}-\x{3096}\x{30A0}-\x{30FF}\x{3400}-\x{4DB5}\x{4E00}-\x{9FCB}\x{F900}-\x{FA6A}\x{2E80}-\x{2FD5}\x{FF66}-\x{FF9F}\x{FFA1}-\x{FFDC}\x{31F0}-\x{31FF}]+$`,
		),
		regexp.MustCompile(`^[\p{L}\p{M}\p{N}\p{Zs}()._-]+$`),
	}
	if name == nil {
		return true
	}
	n := utf8.RuneCountInString(*name)
	if n < 2 || n > 128 {
		return false
	}
	for _, pattern := range clientNamePatterns {
		if !pattern.MatchString(*name) {
			return false
		}
	}
	return true
}

func (f *fakeRauthy) create(w http.ResponseWriter, r *http.Request) {
	if f.failPost {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := requireArrays(raw, "redirect_uris"); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	var req rauthy.NewClientRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validClientName(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid client name")
		return
	}
	if f.raceOnPost != nil {
		cp := *f.raceOnPost
		cp.ID = req.ID
		f.clients[req.ID] = &cp
		f.secrets[req.ID] = "third-party-secret"
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
	f.created++
	f.secrets[req.ID] = fmt.Sprintf("created-secret-%d", f.created)
	if f.commitThenFail {
		writeError(w, http.StatusInternalServerError, "connection reset")
		return
	}
	writeJSON(w, c)
}

func (f *fakeRauthy) byID(w http.ResponseWriter, r *http.Request, id string) {
	c, exists := f.clients[id]
	if !exists {
		writeClientNotFound(w)
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
		delete(f.secrets, id)
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
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req rauthy.UpdateClientRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.lastPut = &req
	f.lastPutRaw = raw
	if !validClientName(req.Name) {
		writeError(w, http.StatusBadRequest, "name must contain at least two characters")
		return
	}
	// Like Rauthy, refuse null for the scope lists and store the default
	// scope unless the stored CSV already contains openid.
	if msg := requireArrays(raw, "scopes", "default_scopes"); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !strings.Contains(strings.Join(req.Scopes, ","), "openid") {
		req.Scopes = append([]string{"openid"}, req.Scopes...)
	}
	if !strings.Contains(strings.Join(req.DefaultScopes, ","), "openid") {
		req.DefaultScopes = append([]string{"openid"}, req.DefaultScopes...)
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
		AllowedResources:       req.AllowedResources,
		DefaultAud:             req.DefaultAud,
		Claims:                 req.Claims,
		ClaimsAtRoot:           req.ClaimsAtRoot,
	}
	f.clients[id] = c
	writeJSON(w, c)
}

// requireArrays returns Rauthy's deserializer error when one of the named
// fields of a JSON object is missing or not an array, or "".
func requireArrays(raw []byte, fields ...string) string {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return err.Error()
	}
	for _, name := range fields {
		v, ok := body[name]
		if !ok {
			return "Json deserialize error: missing field `" + name + "`"
		}
		if !strings.HasPrefix(strings.TrimSpace(string(v)), "[") {
			return "Json deserialize error: invalid type: " + string(v) + ", expected a sequence"
		}
	}
	return ""
}

func (f *fakeRauthy) secret(w http.ResponseWriter, id string) {
	c, exists := f.clients[id]
	if !exists {
		writeClientNotFound(w)
		return
	}
	if f.failSecret {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	resp := rauthy.ClientSecretResponse{ID: id, Confidential: c.Confidential}
	if c.Confidential {
		s := rauthy.NewSecret(f.secrets[id])
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

// Both Rauthy 0.36.2 and 0.37.2 return this envelope for a missing client.
func writeClientNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"timestamp":1794955166,"error":"NotFound","message":"no rows returned"}`)
}
