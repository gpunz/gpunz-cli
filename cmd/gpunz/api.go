package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// APIError is an error envelope returned by the gpu.nz API.
type APIError struct {
	Status     int
	Code       string
	Message    string
	Details    json.RawMessage
	RetryAfter string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// client talks to the REST API. A zero key sends no Authorization header.
type client struct {
	base string
	key  string
	hc   *http.Client
	lang string
	ua   string
}

// do sends one request. The response body is decoded into out (when non-nil)
// and returned raw, so --json can print exactly what the API sent.
func (c *client) do(method, p string, q url.Values, body any, hdr map[string]string, out any) ([]byte, error) {
	u := c.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", c.lang)
	req.Header.Set("User-Agent", c.ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &codeErr{code: exitGeneric, err: fmt.Errorf("cannot reach %s: %v", c.base, unwrapURLErr(err))}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp, data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("unexpected response from API: %v", err)
		}
	}
	return data, nil
}

func unwrapURLErr(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap()
	}
	return err
}

func parseAPIError(resp *http.Response, data []byte) *APIError {
	ae := &APIError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error.Code != "" {
		ae.Code = env.Error.Code
		ae.Message = env.Error.Message
		ae.Details = env.Error.Details
	} else {
		ae.Code = codeForStatus(resp.StatusCode)
	}
	if ae.Message == "" {
		ae.Message = strings.ToLower(http.StatusText(resp.StatusCode))
	}
	return ae
}

func codeForStatus(status int) string {
	switch status {
	case 400, 422:
		return "validation_error"
	case 401:
		return "unauthorized"
	case 402:
		return "insufficient_funds"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 503:
		return "provider_unavailable"
	}
	return "internal"
}

// JSON shapes of the public API (https://gpu.nz/api/v1/openapi.json). Only fields the CLI shows are modelled.

type User struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	Name           *string `json:"name"`
	CreatedAt      string  `json:"created_at"`
	TwoFactor      bool    `json:"two_factor"`
	TelegramLinked bool    `json:"telegram_linked"`
}

type Balance struct {
	BalanceKop     int64    `json:"balance_kop"`
	AvailableKop   int64    `json:"available_kop"`
	CreditLimitKop int64    `json:"credit_limit_kop"`
	BurnKopH       int64    `json:"burn_kop_h"`
	RunwayHours    *float64 `json:"runway_hours"`
	Currency       string   `json:"currency"`
}

type GpuModel struct {
	Slug                  string `json:"slug"`
	Name                  string `json:"name"`
	VRAMGB                int    `json:"vram_gb"`
	Tier                  string `json:"tier"`
	AvailableGpus         int    `json:"available_gpus"`
	FromKopH              *int64 `json:"from_kop_h"`
	FromInterruptibleKopH *int64 `json:"from_interruptible_kop_h"`
}

type GPURef struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
	VRAMGB int    `json:"vram_gb"`
}

type Offer struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Verified  bool   `json:"verified"`
	Country   string `json:"country"`
	GPU       GPURef `json:"gpu"`
	CPU       struct {
		Name  *string `json:"name"`
		Cores *int    `json:"cores"`
	} `json:"cpu"`
	RAM  *int `json:"ram_gb"`
	Disk struct {
		MaxGB *int `json:"max_gb"`
	} `json:"disk"`
	Network struct {
		UpMbps   *int `json:"up_mbps"`
		DownMbps *int `json:"down_mbps"`
	} `json:"network"`
	Reliability *float64 `json:"reliability"`
	Price       struct {
		OnDemandKopH      int64  `json:"on_demand_kop_h"`
		InterruptibleKopH *int64 `json:"interruptible_kop_h"`
	} `json:"price"`
}

type Template struct {
	Slug              string `json:"slug"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Image             string `json:"image"`
	LaunchMode        string `json:"launch_mode"`
	Ports             []int  `json:"ports"`
	MinDiskGB         int    `json:"min_disk_gb"`
	RecommendedDiskGB int    `json:"recommended_disk_gb"`
	Public            bool   `json:"public"`
	Owner             string `json:"owner"`
}

type SshKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

type SSHAccess struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Command string `json:"command"`
}

type URLRef struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Instance struct {
	ID           string  `json:"id"`
	Name         *string `json:"name"`
	Status       string  `json:"status"`
	DesiredState string  `json:"desired_state"`
	Type         string  `json:"type"`
	GPU          GPURef  `json:"gpu"`
	Template     struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"template"`
	Image   string `json:"image"`
	DiskGB  int    `json:"disk_gb"`
	Country string `json:"country"`
	Price   struct {
		RunningKopH  int64 `json:"running_kop_h"`
		StoppedKopH  int64 `json:"stopped_kop_h"`
		TrafficKopGB int64 `json:"traffic_kop_gb"`
	} `json:"price"`
	CostSoFarKop int64      `json:"cost_so_far_kop"`
	SSH          *SSHAccess `json:"ssh"`
	URLs         []URLRef   `json:"urls"`
	CreatedAt    string     `json:"created_at"`
	StartedAt    *string    `json:"started_at"`
	StopReason   *string    `json:"stop_reason"`
	// Currency is the unit of Price and CostSoFarKop: "RUB" (kopecks) or "USD" (1/10 000 $). Absent on older servers.
	Currency string `json:"currency"`
}

type ApiKey struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	Source     string   `json:"source"`
	LastUsedAt *string  `json:"last_used_at"`
	CreatedAt  string   `json:"created_at"`
	ExpiresAt  *string  `json:"expires_at"`
}

type ApiKeyCreated struct {
	ApiKey
	Secret string `json:"secret"`
}

type listEnvelope[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// fxInfo is the rate a catalog list carries beside its data (GET /gpus, GET /offers): rubles per dollar for dollar
// prices, null while none is published.
type fxInfo struct {
	Currency string   `json:"currency"`
	USDRub   *float64 `json:"usd_rub"`
}

// catalogFX reads the fx object of a catalog response. It is nil when the response has none (an older server, or a
// bare array).
func catalogFX(raw []byte) *fxInfo {
	var env struct {
		FX *fxInfo `json:"fx"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	return env.FX
}
