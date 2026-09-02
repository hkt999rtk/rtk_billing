package usagecheckpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	digestDomain     = "mqtt-usage-settlement-checkpoint-v1"
	maximumClockSkew = 30 * time.Second
)

var ErrUnavailable = errors.New("MQTT usage settlement checkpoint unavailable")

type Scope struct {
	CloudID          string    `json:"cloud_id"`
	OwnerUserID      string    `json:"owner_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	CoveredThrough   time.Time `json:"covered_through"`
}

type Evidence struct {
	Scope
	Complete               bool      `json:"complete"`
	ReceiptID              string    `json:"receipt_id"`
	AppliedEvents          int64     `json:"applied_events"`
	LegacyWindows          int64     `json:"legacy_windows"`
	SourceCheckpointSHA256 string    `json:"source_checkpoint_sha256"`
	CheckpointSHA256       string    `json:"checkpoint_sha256"`
	ObservedAt             time.Time `json:"observed_at"`
	ExpiresAt              time.Time `json:"expires_at"`
}

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func New(baseURL, token string, transport http.RoundTripper) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") || len(token) < 32 || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") || !trustedOrigin(u) {
		return nil, ErrUnavailable
	}
	u.Path = "/v1/internal/mqtt-usage-settlement/checkpoint"
	return &Client{endpoint: u.String(), token: token, http: &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}, nil
}

func trustedOrigin(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".svc.cluster.local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) Checkpoint(ctx context.Context, scope Scope) (Evidence, error) {
	coveredThrough := scope.CoveredThrough.UTC().Truncate(time.Microsecond)
	if c == nil || c.http == nil || !canonicalUUID(scope.CloudID) || !canonicalUUID(scope.OwnerUserID) || scope.OwnershipVersion <= 0 ||
		scope.CoveredThrough.IsZero() || !scope.CoveredThrough.Equal(coveredThrough) {
		return Evidence{}, ErrUnavailable
	}
	// pgx may scan a UTC timestamptz using a zero-offset location other than
	// time.UTC. Canonicalize before JSON round-tripping so the response remains
	// structurally bound to the request without rejecting the same instant.
	scope.CoveredThrough = coveredThrough
	raw, err := json.Marshal(scope)
	if err != nil {
		return Evidence{}, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return Evidence{}, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Evidence{}, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return Evidence{}, ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return Evidence{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence Evidence
	now := time.Now().UTC()
	if decoder.Decode(&evidence) != nil || decoder.Decode(new(any)) != io.EOF || evidence.Scope != scope || !evidence.Complete || !canonicalUUID(evidence.ReceiptID) || evidence.AppliedEvents < 0 || evidence.LegacyWindows < 0 ||
		!validDigest(evidence.SourceCheckpointSHA256) || evidence.CheckpointSHA256 != evidence.digest() || evidence.ObservedAt.After(now.Add(maximumClockSkew)) || !evidence.ObservedAt.After(now.Add(-5*time.Minute)) ||
		!evidence.ExpiresAt.After(now) || evidence.ExpiresAt.After(evidence.ObservedAt.Add(5*time.Minute)) {
		return Evidence{}, ErrUnavailable
	}
	return evidence, nil
}

func canonicalUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid && id.Bytes != [16]byte{} && id.String() == value
}

func (e Evidence) digest() string {
	fields := []string{digestDomain, e.CloudID, e.OwnerUserID, strconv.FormatInt(e.OwnershipVersion, 10), e.CoveredThrough.UTC().Format(time.RFC3339Nano),
		strconv.FormatBool(e.Complete), e.ReceiptID, strconv.FormatInt(e.AppliedEvents, 10), strconv.FormatInt(e.LegacyWindows, 10), e.SourceCheckpointSHA256,
		e.ObservedAt.UTC().Format(time.RFC3339Nano), e.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	hash := sha256.New()
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
