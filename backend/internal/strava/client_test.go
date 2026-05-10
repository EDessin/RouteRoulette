package strava

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExchangeCodeStoresToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %s, want /oauth/token", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() returned error: %v", err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "abc" {
			t.Fatalf("form = %v, want authorization_code abc", r.Form)
		}
		_ = json.NewEncoder(w).Encode(Token{
			AccessToken:  "access",
			RefreshToken: "refresh",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()
	client := NewClient(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/callback",
		DataDir:      t.TempDir(),
		TokenURL:     server.URL + "/oauth/token",
	})

	if _, err := client.ExchangeCode(context.Background(), "abc"); err != nil {
		t.Fatalf("ExchangeCode() returned error: %v", err)
	}
	token, err := client.LoadToken()
	if err != nil {
		t.Fatalf("LoadToken() returned error: %v", err)
	}
	if token.AccessToken != "access" || token.RefreshToken != "refresh" {
		t.Fatalf("token = %+v, want stored token", token)
	}
}

func TestAccessTokenRefreshesExpiredToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() returned error: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("form = %v, want refresh_token old-refresh", r.Form)
		}
		_ = json.NewEncoder(w).Encode(Token{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()
	client := NewClient(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/callback",
		DataDir:      t.TempDir(),
		TokenURL:     server.URL,
	})
	if err := client.SaveToken(Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1}); err != nil {
		t.Fatalf("SaveToken() returned error: %v", err)
	}

	token, err := client.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() returned error: %v", err)
	}
	if token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" {
		t.Fatalf("token = %+v, want refreshed token", token)
	}
}

func TestActivityLatLngStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.String(), "/activities/42/streams") {
			t.Fatalf("url = %s, want activity stream URL", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Fatalf("Authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"latlng": map[string]any{
				"data": [][]float64{{50.0, 4.0}, {50.1, 4.1}},
			},
		})
	}))
	defer server.Close()
	client := NewClient(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/callback",
		DataDir:      t.TempDir(),
		APIBaseURL:   server.URL,
	})
	if err := client.SaveToken(Token{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatalf("SaveToken() returned error: %v", err)
	}

	coords, err := client.ActivityLatLngStream(context.Background(), 42)
	if err != nil {
		t.Fatalf("ActivityLatLngStream() returned error: %v", err)
	}
	if len(coords) != 2 || coords[0].Lat != 50 || coords[1].Lon != 4.1 {
		t.Fatalf("coords = %+v, want latlng stream coordinates", coords)
	}
}
