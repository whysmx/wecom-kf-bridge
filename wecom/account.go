package wecom

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type Account struct {
	OpenKfID      string `json:"open_kfid"`
	Name          string `json:"name,omitempty"`
	AvatarMediaID string `json:"avatar_media_id,omitempty"`
	URL           string `json:"url,omitempty"`
}
type AccountAddRequest struct {
	Name    string `json:"name"`
	MediaID string `json:"media_id,omitempty"`
}
type AccountUpdateRequest struct {
	OpenKfID string `json:"open_kfid"`
	Name     string `json:"name,omitempty"`
	MediaID  string `json:"media_id,omitempty"`
}
type AccountListRequest struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}
type AccountListResponse struct {
	ErrCode  int       `json:"errcode"`
	ErrMsg   string    `json:"errmsg"`
	Accounts []Account `json:"account_list"`
	Total    int       `json:"total"`
}
type AccountResponse struct {
	ErrCode  int    `json:"errcode"`
	ErrMsg   string `json:"errmsg"`
	OpenKfID string `json:"open_kfid"`
	URL      string `json:"url"`
}
type ServiceStateRequest struct {
	OpenKfID       string `json:"open_kfid"`
	ExternalUserID string `json:"external_userid"`
	ServiceState   int    `json:"service_state,omitempty"`
	ServicerUserID string `json:"servicer_userid,omitempty"`
}
type ServiceStateResponse struct {
	ErrCode        int    `json:"errcode"`
	ErrMsg         string `json:"errmsg"`
	ServiceState   int    `json:"service_state"`
	ServicerUserID string `json:"servicer_userid"`
}
type Servicer struct {
	UserID string `json:"userid"`
	Name   string `json:"name"`
}
type ServicerListResponse struct {
	ErrCode   int        `json:"errcode"`
	ErrMsg    string     `json:"errmsg"`
	Servicers []Servicer `json:"servicer_list"`
}

func (c *Client) AddAccount(ctx context.Context, token string, req AccountAddRequest) (AccountResponse, error) {
	if strings.TrimSpace(req.Name) == "" {
		return AccountResponse{}, fmt.Errorf("%w: account name required", ErrInvalidArgument)
	}
	var out AccountResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/account/add", token, nil, req, &out)
	return out, err
}
func (c *Client) UpdateAccount(ctx context.Context, token string, req AccountUpdateRequest) (AccountResponse, error) {
	if strings.TrimSpace(req.OpenKfID) == "" {
		return AccountResponse{}, fmt.Errorf("%w: open_kfid required", ErrInvalidArgument)
	}
	var out AccountResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/account/update", token, nil, req, &out)
	return out, err
}
func (c *Client) DeleteAccount(ctx context.Context, token, openKfID string) error {
	if strings.TrimSpace(openKfID) == "" {
		return fmt.Errorf("%w: open_kfid required", ErrInvalidArgument)
	}
	return c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/account/del", token, nil, struct {
		OpenKfID string `json:"open_kfid"`
	}{openKfID}, &struct {
		ErrCode int `json:"errcode"`
	}{})
}
func (c *Client) ListAccounts(ctx context.Context, token string, req AccountListRequest) (AccountListResponse, error) {
	if req.Offset < 0 || req.Limit < 0 {
		return AccountListResponse{}, fmt.Errorf("%w: invalid pagination", ErrInvalidArgument)
	}
	var out AccountListResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/account/list", token, nil, req, &out)
	return out, err
}
func (c *Client) AddContactWay(ctx context.Context, token string, req map[string]any) (AccountResponse, error) {
	var out AccountResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/add_contact_way", token, nil, req, &out)
	return out, err
}
func (c *Client) GetServiceState(ctx context.Context, token string, req ServiceStateRequest) (ServiceStateResponse, error) {
	if req.OpenKfID == "" || req.ExternalUserID == "" {
		return ServiceStateResponse{}, fmt.Errorf("%w: service identity required", ErrInvalidArgument)
	}
	var out ServiceStateResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/service_state/get", token, nil, req, &out)
	return out, err
}
func (c *Client) TransServiceState(ctx context.Context, token string, req ServiceStateRequest) (ServiceStateResponse, error) {
	if req.OpenKfID == "" || req.ExternalUserID == "" {
		return ServiceStateResponse{}, fmt.Errorf("%w: service identity required", ErrInvalidArgument)
	}
	var out ServiceStateResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/service_state/trans", token, nil, req, &out)
	return out, err
}
func (c *Client) ListServicers(ctx context.Context, token, openKfID string) (ServicerListResponse, error) {
	if openKfID == "" {
		return ServicerListResponse{}, fmt.Errorf("%w: open_kfid required", ErrInvalidArgument)
	}
	q := url.Values{"open_kfid": []string{openKfID}}
	var out ServicerListResponse
	err := c.doJSON(ctx, http.MethodGet, "/cgi-bin/kf/servicer/list", token, q, nil, &out)
	return out, err
}
