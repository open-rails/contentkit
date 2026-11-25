package endpoint

import "testing"

func TestSecure(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://contentkit.example/api":     true,
		"http://localhost:8080/api":          true,
		"http://LOCALHOST/api":               true,
		"http://127.0.0.1:9000":              true,
		"http://127.8.9.10":                  true,
		"http://[::1]:8080":                  true,
		"http://contentkit.example/api":      false,
		"http://10.0.0.5:8080":               false,
		"http://localhost.example":           false,
		"http://[::ffff:10.0.0.1]":           false,
		"ftp://contentkit.example":           false,
		"https://user:pw@contentkit.example": false,
		"/api":                               false,
		"":                                   false,
	} {
		if _, err := Secure(raw); (err == nil) != ok {
			t.Errorf("Secure(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}
