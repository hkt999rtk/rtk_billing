package otagrant

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

var ErrUnverified = errors.New("OTA Product grant cannot be verified")

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

type historicalGrant struct {
	BrandCloudID           string     `json:"brand_cloud_id"`
	ProductID              string     `json:"product_id"`
	ServiceCode            string     `json:"service_code"`
	Enabled                bool       `json:"enabled"`
	ProductServiceRevision int64      `json:"product_service_revision"`
	ServiceGrantSHA256     string     `json:"service_grant_sha256"`
	ValidFrom              time.Time  `json:"valid_from"`
	ValidUntil             *time.Time `json:"valid_until,omitempty"`
}

type billingTierPeriod struct {
	BrandCloudID            string     `json:"brand_cloud_id"`
	Tier                    string     `json:"tier"`
	CoveredFrom             *time.Time `json:"covered_from,omitempty"`
	HistoryCoversStart      bool       `json:"history_covers_start"`
	ChangedWithinPeriod     bool       `json:"changed_within_period"`
	CommercialForFullPeriod bool       `json:"commercial_for_full_period"`
}

func NewClient(baseURL, token string, client *http.Client) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && (base.Scheme != "http" || !localHTTP(base.Hostname()))) ||
		len(token) < 32 || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return nil, ErrUnverified
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	clone := *client
	if clone.Timeout <= 0 || clone.Timeout > 10*time.Second {
		clone.Timeout = 10 * time.Second
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: base, token: token, http: &clone}, nil
}

func localHTTP(host string) bool {
	return strings.EqualFold(host, "localhost") ||
		(net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) ||
		strings.HasSuffix(strings.ToLower(host), ".svc.cluster.local")
}

func (c *Client) VerifyOTAGrant(ctx context.Context, cloudID, productID string, witness billing.OTAGrantEvidence) error {
	if c == nil || c.base == nil || c.http == nil || cloudID == "" || productID == "" ||
		witness.ProductServiceRevision < 1 || len(witness.ServiceGrantSHA256) != 64 || witness.AuthorizedAt.IsZero() {
		return ErrUnverified
	}
	if _, err := hex.DecodeString(witness.ServiceGrantSHA256); err != nil {
		return ErrUnverified
	}
	endpoint := *c.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/internal/brand-clouds/" +
		url.PathEscape(cloudID) + "/products/" + url.PathEscape(productID) +
		"/ota-grants/" + strconv.FormatInt(witness.ProductServiceRevision, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: request", ErrUnverified)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: historical lookup unavailable", ErrUnverified)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: historical lookup HTTP %d", ErrUnverified, resp.StatusCode)
	}
	var grant historicalGrant
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil || decoder.Decode(new(any)) != io.EOF ||
		grant.BrandCloudID != cloudID || grant.ProductID != productID || grant.ServiceCode != "ota" ||
		!grant.Enabled || grant.ProductServiceRevision != witness.ProductServiceRevision ||
		grant.ServiceGrantSHA256 != witness.ServiceGrantSHA256 || grant.ValidFrom.IsZero() ||
		witness.AuthorizedAt.Before(grant.ValidFrom) ||
		grant.ValidUntil != nil && !witness.AuthorizedAt.Before(*grant.ValidUntil) {
		return ErrUnverified
	}
	return nil
}

// VerifyCommercialMonth obtains Account Manager's immutable tier evidence for
// the entire UTC month. A current tier cannot prove an earlier billing period.
func (c *Client) VerifyCommercialMonth(ctx context.Context, cloudID string, start, end time.Time) error {
	if c == nil || c.base == nil || c.http == nil || cloudID == "" ||
		start.Location() != time.UTC || end.Location() != time.UTC ||
		!start.Equal(time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)) ||
		!end.Equal(start.AddDate(0, 1, 0)) {
		return ErrUnverified
	}
	endpoint := *c.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/internal/brand-clouds/" +
		url.PathEscape(cloudID) + "/billing-tier"
	query := endpoint.Query()
	query.Set("period_start", start.Format(time.RFC3339Nano))
	query.Set("period_end", end.Format(time.RFC3339Nano))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: request", ErrUnverified)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: tier history unavailable", ErrUnverified)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: tier history HTTP %d", ErrUnverified, resp.StatusCode)
	}
	var period billingTierPeriod
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&period); err != nil || decoder.Decode(new(any)) != io.EOF ||
		period.BrandCloudID != cloudID || period.Tier != "commercial" ||
		period.CoveredFrom == nil || period.CoveredFrom.After(start) ||
		!period.HistoryCoversStart || period.ChangedWithinPeriod || !period.CommercialForFullPeriod {
		return ErrUnverified
	}
	return nil
}
