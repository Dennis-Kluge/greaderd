package greader

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// deriveTokens derives the tokens handed to clients from the credentials, so
// they survive restarts and become invalid when the password changes.
func deriveTokens(c Credentials) (auth, write string) {
	sign := func(purpose string) string {
		mac := hmac.New(sha256.New, []byte(c.Password))
		mac.Write([]byte(purpose + ":" + c.User))
		return hex.EncodeToString(mac.Sum(nil))
	}
	// The write token has the length of FreshRSS's tokens.
	return c.User + "/" + sign("auth"), sign("token")[:57]
}

// requireAuth rejects requests without a valid
// "Authorization: GoogleLogin auth=<token>" header and limits the body size.
func (s *Server) requireAuth(h apiFunc) apiFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "GoogleLogin auth=")
		if !ok || !secureEqual(token, s.authToken) {
			return errorf(http.StatusUnauthorized, "Unauthorized")
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		return h(w, r)
	}
}

// requireWriteToken additionally requires the T token from /token, the
// protocol's protection against cross-site requests.
func (s *Server) requireWriteToken(h apiFunc) apiFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) error {
		if err := r.ParseForm(); err != nil {
			return errorf(http.StatusBadRequest, "invalid form: %v", err)
		}
		if !secureEqual(r.Form.Get("T"), s.writeToken) {
			// FreshRSS sends this header; NetNewsWire fetches a new token on any 401.
			w.Header().Set("Google-Bad-Token", "true")
			return errorf(http.StatusUnauthorized, "Unauthorized")
		}
		return h(w, r)
	})
}

// clientLogin handles POST /accounts/ClientLogin with the form fields Email
// and Passwd. The Auth value of the response authenticates later requests.
// Clients with too many failed attempts get 429 until their window expires.
func (s *Server) clientLogin(w http.ResponseWriter, r *http.Request) error {
	client := clientIP(r, s.trustProxy)
	if wait, blocked := s.logins.blocked(client); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		return errorf(http.StatusTooManyRequests, "Error=TooManyAttempts")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		return errorf(http.StatusBadRequest, "invalid form: %v", err)
	}
	// Compare both values so the response time does not reveal which one is wrong.
	userOK := secureEqual(r.PostForm.Get("Email"), s.creds.User)
	passOK := secureEqual(r.PostForm.Get("Passwd"), s.creds.Password)
	if !userOK || !passOK {
		s.logins.fail(client)
		s.log.Warn("failed login", "client", client)
		return errorf(http.StatusUnauthorized, "Error=BadAuthentication")
	}
	s.logins.succeed(client)
	writeText(w, fmt.Sprintf("SID=%s\nLSID=null\nAuth=%s\n", s.authToken, s.authToken))
	return nil
}

// token handles GET /reader/api/0/token.
func (s *Server) token(w http.ResponseWriter, _ *http.Request) error {
	writeText(w, s.writeToken+"\n")
	return nil
}

// userInfo handles GET /reader/api/0/user-info.
func (s *Server) userInfo(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, userInfoResponse{
		UserID:        "1",
		UserName:      s.creds.User,
		UserProfileID: "1",
		UserEmail:     s.creds.User,
	})
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
