package critical_test

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
)

// The route data is the approved plan's current inventory, not discovered
// from the same running router whose missing authentication this detects.
// Every row below sends an actual request and reports its own refusal result.
// Open operations deliberately have no credential gate and are not counted
// as authentication coverage. Positive/error/functionality cases stay separate.
func TestHTTPAuthenticationMatrix(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	item := publishedArticle(t, a, "HTTP authentication target")
	user, revoked := w.person(t, 0)
	claims, err := w.tenant[0].Verify(t.Context(), revoked)
	must(t, err)
	identity := authtest.Identity(t, w.tenant[0], revoked)
	must(t, w.tenant[0].RevokeSession(t.Context(), identity, user.ID, claims.SessionID))
	_, member := w.person(t, 0)
	_, err = a.client(member).GetContents(t.Context(), ck.GetContentsQuery{})
	must(t, err)
	signed := func(change func(map[string]any)) string {
		parts := strings.Split(member, ".")
		if len(parts) != 3 {
			t.Fatal("issuer did not return a JWT")
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		must(t, err)
		var claims map[string]any
		must(t, json.Unmarshal(raw, &claims))
		change(claims)
		raw, err = json.Marshal(claims)
		must(t, err)
		input := parts[0] + "." + base64.RawURLEncoding.EncodeToString(raw)
		signature, err := w.signer.Sign(t.Context(), []byte(input))
		must(t, err)
		return input + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	expired := signed(func(claims map[string]any) {
		claims["iat"] = time.Now().Add(-time.Hour).Unix()
		claims["exp"] = time.Now().Add(-2 * time.Minute).Unix()
	})
	wrongIssuer := signed(func(claims map[string]any) { claims["iss"] = "https://wrong-issuer.critical.invalid" })
	appKey, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), w.apps[1].group, iam.NewAPIKey{Name: "matrix sibling app", Role: w.writer})
	must(t, err)
	_, err = w.apps[1].client(appKey.Secret).GetContents(t.Context(), ck.GetContentsQuery{})
	must(t, err)

	file, err := os.Open("http_matrix.tsv")
	must(t, err)
	defer file.Close()
	reader := csv.NewReader(file)
	reader.Comma = '\t'
	rows, err := reader.ReadAll()
	must(t, err)
	if len(rows) < 2 {
		t.Fatal("required HTTP operation inventory is empty")
	}
	parameter := regexp.MustCompile(`\{([^}]+)\}`)
	for _, row := range rows[1:] {
		id, method, path, auth := row[0], row[1], row[2], row[3]
		if auth == "open" {
			continue
		}
		path = parameter.ReplaceAllStringFunc(path, func(slot string) string {
			switch slot {
			case "{kind}":
				return "gallery"
			case "{id}":
				return item.ID
			case "{vid}", "{version}":
				return item.Versions[0].ID
			case "{language}":
				return "en"
			case "{path...}":
				return "reader/master.m3u8"
			default:
				return item.ID
			}
		})
		t.Run(id+"_"+method+"_"+row[2], func(t *testing.T) {
			probe := func(t *testing.T, method, bearer string, status int, code string) {
				t.Helper()
				request, err := http.NewRequestWithContext(t.Context(), method, a.server.URL+"/content"+path, strings.NewReader("{}"))
				must(t, err)
				request.Header.Set("Content-Type", "application/json")
				if bearer != "" {
					request.Header.Set("Authorization", "Bearer "+bearer)
				}
				response, err := http.DefaultClient.Do(request)
				must(t, err)
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				must(t, err)
				if response.StatusCode != status {
					t.Fatalf("%s %s returned %d, want %d; body=%s", method, row[2], response.StatusCode, status, body)
				}
				if status == 401 && !strings.HasPrefix(response.Header.Get("WWW-Authenticate"), "Bearer") {
					t.Fatalf("credential refusal omitted challenge: %v", response.Header)
				}
				if method != "HEAD" {
					var result struct {
						Code string `json:"code"`
					}
					must(t, json.Unmarshal(body, &result))
					if result.Code != code {
						t.Fatalf("%s %s returned code %q, want %q", method, row[2], result.Code, code)
					}
				}
				if strings.Contains(string(body), "Published body: HTTP authentication target") {
					t.Fatal("credential refusal disclosed protected target body")
				}
			}
			t.Run("malformed_credential", func(t *testing.T) { probe(t, method, "not-a-credential", 401, "unauthorized") })
			t.Run("revoked_session", func(t *testing.T) { probe(t, method, revoked, 401, "unauthorized") })
			t.Run("expired_credential", func(t *testing.T) { probe(t, method, expired, 401, "unauthorized") })
			t.Run("wrong_issuer", func(t *testing.T) { probe(t, method, wrongIssuer, 401, "unauthorized") })
			t.Run("foreign_app_key", func(t *testing.T) { probe(t, method, appKey.Secret, 403, "forbidden") })
			if auth != "public" {
				t.Run("anonymous_protected_operation", func(t *testing.T) { probe(t, method, "", 401, "unauthorized") })
			}

			if method == "GET" {
				t.Run("inherited_HEAD_credential_gate", func(t *testing.T) { probe(t, "HEAD", "not-a-credential", 401, "unauthorized") })
			}
			if auth == "staff" {
				t.Run("member_without_named_permission", func(t *testing.T) { probe(t, method, member, 403, "forbidden") })
			}
		})
	}
}
