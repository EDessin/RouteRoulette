package strava

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

const (
	defaultAPIBaseURL = "https://www.strava.com/api/v3"
	defaultAuthURL    = "https://www.strava.com/oauth/authorize"
	defaultTokenURL   = "https://www.strava.com/oauth/token"
)

type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	DataDir      string
	APIBaseURL   string
	AuthURL      string
	TokenURL     string
}

type Client struct {
	cfg        Config
	httpClient *http.Client
}

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Scope        string `json:"scope,omitempty"`
}

type Activity struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	SportType    string  `json:"sport_type"`
	StartDate    string  `json:"start_date"`
	DistanceM    float64 `json:"distance"`
	HasHeartrate bool    `json:"has_heartrate,omitempty"`
}

type SyncResult struct {
	FetchedActivities int    `json:"fetchedActivities"`
	SkippedActivities int    `json:"skippedActivities"`
	SyncedActivities  int    `json:"syncedActivities"`
	LastSyncAt        string `json:"lastSyncAt,omitempty"`
}

func NewClient(cfg Config) Client {
	if cfg.DataDir == "" {
		cfg.DataDir = "data/history"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultAPIBaseURL
	}
	if cfg.AuthURL == "" {
		cfg.AuthURL = defaultAuthURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = defaultTokenURL
	}
	return Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c Client) Configured() bool {
	return c.cfg.ClientID != "" && c.cfg.ClientSecret != "" && c.cfg.RedirectURL != ""
}

func (c Client) Connected() bool {
	token, err := c.LoadToken()
	return err == nil && token.RefreshToken != ""
}

func (c Client) AuthorizationURL() (string, error) {
	if !c.Configured() {
		return "", errors.New("Strava is not configured; set STRAVA_CLIENT_ID and STRAVA_CLIENT_SECRET")
	}
	values := url.Values{}
	values.Set("client_id", c.cfg.ClientID)
	values.Set("redirect_uri", c.cfg.RedirectURL)
	values.Set("response_type", "code")
	values.Set("approval_prompt", "auto")
	values.Set("scope", "activity:read_all")
	return c.cfg.AuthURL + "?" + values.Encode(), nil
}

func (c Client) ExchangeCode(ctx context.Context, code string) (Token, error) {
	if strings.TrimSpace(code) == "" {
		return Token{}, errors.New("Strava callback did not include an authorization code")
	}
	var token Token
	if err := c.postToken(ctx, url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
	}, &token); err != nil {
		return Token{}, err
	}
	if err := c.SaveToken(token); err != nil {
		return Token{}, err
	}
	return token, nil
}

func (c Client) AccessToken(ctx context.Context) (Token, error) {
	token, err := c.LoadToken()
	if err != nil {
		return Token{}, err
	}
	if token.AccessToken != "" && token.ExpiresAt > time.Now().Add(time.Minute).Unix() {
		return token, nil
	}

	var refreshed Token
	if err := c.postToken(ctx, url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"refresh_token": {token.RefreshToken},
		"grant_type":    {"refresh_token"},
	}, &refreshed); err != nil {
		return Token{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	if err := c.SaveToken(refreshed); err != nil {
		return Token{}, err
	}
	return refreshed, nil
}

func (c Client) ListActivities(ctx context.Context, page int, perPage int) ([]Activity, error) {
	if perPage <= 0 || perPage > 200 {
		perPage = 200
	}
	token, err := c.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(c.cfg.APIBaseURL + "/athlete/activities")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(perPage))
	endpoint.RawQuery = query.Encode()

	var activities []Activity
	if err := c.getJSON(ctx, endpoint.String(), token.AccessToken, &activities); err != nil {
		return nil, err
	}
	return activities, nil
}

func (c Client) ActivityLatLngStream(ctx context.Context, id int64) ([]planner.Coordinate, error) {
	token, err := c.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(fmt.Sprintf("%s/activities/%d/streams", c.cfg.APIBaseURL, id))
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("keys", "latlng")
	query.Set("key_by_type", "true")
	endpoint.RawQuery = query.Encode()

	var streams struct {
		LatLng struct {
			Data [][]float64 `json:"data"`
		} `json:"latlng"`
	}
	if err := c.getJSON(ctx, endpoint.String(), token.AccessToken, &streams); err != nil {
		return nil, err
	}
	coords := make([]planner.Coordinate, 0, len(streams.LatLng.Data))
	for _, point := range streams.LatLng.Data {
		if len(point) < 2 {
			continue
		}
		coords = append(coords, planner.Coordinate{Lat: point[0], Lon: point[1]})
	}
	return coords, nil
}

func IsRunActivity(activity Activity) bool {
	sportType := strings.ToLower(activity.SportType)
	activityType := strings.ToLower(activity.Type)
	return strings.Contains(sportType, "run") || strings.Contains(activityType, "run")
}

func (c Client) LoadToken() (Token, error) {
	file, err := os.Open(c.tokenPath())
	if err != nil {
		return Token{}, err
	}
	defer file.Close()
	var token Token
	if err := json.NewDecoder(file).Decode(&token); err != nil {
		return Token{}, err
	}
	return token, nil
}

func (c Client) SaveToken(token Token) error {
	if err := os.MkdirAll(c.cfg.DataDir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.tokenPath(), body, 0o600)
}

func (c Client) DeleteToken() error {
	if err := os.Remove(c.tokenPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c Client) postToken(ctx context.Context, values url.Values, target any) error {
	if !c.Configured() {
		return errors.New("Strava is not configured; set STRAVA_CLIENT_ID and STRAVA_CLIENT_SECRET")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, bytes.NewBufferString(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Strava token request returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func (c Client) getJSON(ctx context.Context, endpoint string, accessToken string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Strava request returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func (c Client) tokenPath() string {
	return filepath.Join(c.cfg.DataDir, "strava-token.json")
}
