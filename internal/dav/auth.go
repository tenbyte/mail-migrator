package dav

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type authTransport struct {
	base               http.RoundTripper
	method             domain.DAVAuthMethod
	username, password string
	originHost         string
	mu                 sync.Mutex
	nonceCount         uint32
}

func (t *authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.EqualFold(request.URL.Host, t.originHost) {
		return nil, errors.New("refusing to send DAV credentials to an unexpected host")
	}
	first, err := cloneRequest(request)
	if err != nil {
		return nil, err
	}
	if t.method != domain.DAVAuthDigest {
		first.SetBasicAuth(t.username, t.password)
	}
	response, err := t.base.RoundTrip(first)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	challenge := digestChallenge(response.Header.Values("WWW-Authenticate"))
	if challenge == "" || t.method == domain.DAVAuthBasic {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	retry, err := cloneRequest(request)
	if err != nil {
		return nil, err
	}
	authorization, err := t.authorization(retry, challenge)
	if err != nil {
		return nil, err
	}
	retry.Header.Set("Authorization", authorization)
	return t.base.RoundTrip(retry)
}

func cloneRequest(request *http.Request) (*http.Request, error) {
	clone := request.Clone(request.Context())
	if request.Body == nil {
		return clone, nil
	}
	if request.GetBody == nil {
		return nil, errors.New("DAV authentication retry requires a replayable request body")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	clone.Body = body
	return clone, nil
}

func digestChallenge(headers []string) string {
	for _, header := range headers {
		lower := strings.ToLower(header)
		if index := strings.Index(lower, "digest "); index >= 0 {
			return strings.TrimSpace(header[index+len("digest "):])
		}
	}
	return ""
}

func (t *authTransport) authorization(request *http.Request, challenge string) (string, error) {
	values := parseAuthParams(challenge)
	realm, nonce := values["realm"], values["nonce"]
	if realm == "" || nonce == "" {
		return "", errors.New("invalid digest authentication challenge")
	}
	algorithm := strings.ToUpper(values["algorithm"])
	if algorithm == "" {
		algorithm = "MD5"
	}
	if algorithm != "MD5" && algorithm != "SHA-256" {
		return "", fmt.Errorf("unsupported digest algorithm %s", algorithm)
	}
	qop := ""
	for _, candidate := range strings.Split(values["qop"], ",") {
		if strings.EqualFold(strings.TrimSpace(candidate), "auth") {
			qop = "auth"
			break
		}
	}
	if values["qop"] != "" && qop == "" {
		return "", errors.New("DAV server only offers unsupported digest auth-int")
	}
	cnonceBytes := make([]byte, 12)
	if _, err := rand.Read(cnonceBytes); err != nil {
		return "", err
	}
	cnonce := hex.EncodeToString(cnonceBytes)
	t.mu.Lock()
	t.nonceCount++
	nc := fmt.Sprintf("%08x", t.nonceCount)
	t.mu.Unlock()
	digest := func(value string) string {
		if algorithm == "SHA-256" {
			sum := sha256.Sum256([]byte(value))
			return hex.EncodeToString(sum[:])
		}
		sum := md5.Sum([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	uri := request.URL.RequestURI()
	ha1 := digest(t.username + ":" + realm + ":" + t.password)
	ha2 := digest(request.Method + ":" + uri)
	response := digest(ha1 + ":" + nonce + ":" + ha2)
	if qop != "" {
		response = digest(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	}
	parts := []string{
		`username="` + quoteAuth(t.username) + `"`,
		`realm="` + quoteAuth(realm) + `"`,
		`nonce="` + quoteAuth(nonce) + `"`,
		`uri="` + quoteAuth(uri) + `"`,
		`response="` + response + `"`,
		`algorithm=` + algorithm,
	}
	if opaque := values["opaque"]; opaque != "" {
		parts = append(parts, `opaque="`+quoteAuth(opaque)+`"`)
	}
	if qop != "" {
		parts = append(parts, "qop="+qop, "nc="+nc, `cnonce="`+cnonce+`"`)
	}
	return "Digest " + strings.Join(parts, ", "), nil
}

func parseAuthParams(input string) map[string]string {
	result := map[string]string{}
	start, quoted, escaped := 0, false, false
	consume := func(part string) {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = strings.ReplaceAll(strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`), `\\`, `\`)
		}
		result[strings.ToLower(strings.TrimSpace(key))] = value
	}
	for index, char := range input {
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quoted {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if char == ',' && !quoted {
			consume(input[start:index])
			start = index + 1
		}
	}
	consume(input[start:])
	return result
}

func quoteAuth(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`)
}

func safeRedirect(origin *url.URL) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many DAV redirects")
		}
		if request.URL.Scheme != "https" {
			return errors.New("refusing DAV redirect without TLS")
		}
		if !strings.EqualFold(request.URL.Host, origin.Host) {
			return fmt.Errorf("DAV redirect to unexpected host %s; enter the final URL manually", request.URL.Host)
		}
		return nil
	}
}
