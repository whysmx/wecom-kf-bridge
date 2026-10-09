package wecom

import (
	"context"
	"fmt"
	"net/http"

	"github.com/whysmx/wecom-kf-bridge/state"
	"strings"
	"time"
)

// CustomerBatchGetRequest follows 95166: customer lookup is by external
// userid, never by nickname. The API accepts a bounded list.
type CustomerBatchGetRequest struct {
	ExternalUserIDs    []string `json:"external_userid_list"`
	ExternalUserIDList []string `json:"-"`
}
type CustomerProfile struct {
	ExternalUserID string         `json:"external_userid"`
	Nickname       string         `json:"nickname"`
	Avatar         string         `json:"avatar,omitempty"`
	Gender         int            `json:"gender,omitempty"`
	UnionID        string         `json:"unionid,omitempty"`
	Raw            map[string]any `json:"-"`
}
type CustomerBatchGetResponse struct {
	ErrCode                int               `json:"errcode"`
	ErrMsg                 string            `json:"errmsg"`
	Customers              []CustomerProfile `json:"customer_list"`
	InvalidExternalUserIDs []string          `json:"invalid_external_userid"`
}

func (r CustomerBatchGetRequest) ids() []string {
	if len(r.ExternalUserIDs) > 0 {
		return r.ExternalUserIDs
	}
	return r.ExternalUserIDList
}
func (c *Client) CustomerBatchGet(ctx context.Context, accessToken string, req CustomerBatchGetRequest) (CustomerBatchGetResponse, error) {
	ids := req.ids()
	if strings.TrimSpace(accessToken) == "" || len(ids) == 0 {
		return CustomerBatchGetResponse{}, fmt.Errorf("%w: access token and external_userid_list required", ErrInvalidArgument)
	}
	clean := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return CustomerBatchGetResponse{}, fmt.Errorf("%w: empty external userid", ErrInvalidArgument)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	body := struct {
		ExternalUserIDList []string `json:"external_userid_list"`
	}{clean}
	var out CustomerBatchGetResponse
	if err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/customer/batchget", accessToken, nil, body, &out); err != nil {
		return out, err
	}
	return out, nil
}
func (c *Client) BatchGetCustomer(ctx context.Context, accessToken string, ids []string) (CustomerBatchGetResponse, error) {
	return c.CustomerBatchGet(ctx, accessToken, CustomerBatchGetRequest{ExternalUserIDs: ids})
}

// SyncCustomerNicknames obtains profiles and applies only non-empty nickname
// values. The state package integration is kept in a separate helper so this
// API package remains independent of the durable store.
type CustomerNicknameStore interface {
	SetNickname(context.Context, string, string, time.Time) error
}

// SyncCustomerProfiles bridges the 95166 response to the durable identity
// store. A missing/invalid profile is reported in InvalidExternalUserIDs; an
// empty nickname leaves the cached value untouched.
func (c *Client) SyncCustomerProfiles(ctx context.Context, accessToken string, store *state.Store, enterpriseID, bindingID string, externalUserIDs []string) ([]state.Customer, []string, error) {
	if store == nil {
		return nil, nil, fmt.Errorf("%w: nil state store", ErrInvalidArgument)
	}
	resp, err := c.BatchGetCustomer(ctx, accessToken, externalUserIDs)
	if err != nil {
		return nil, nil, err
	}
	out := make([]state.Customer, 0, len(resp.Customers))
	for _, p := range resp.Customers {
		if strings.TrimSpace(p.ExternalUserID) == "" {
			continue
		}
		cust, e := store.EnsureCustomer(ctx, enterpriseID, bindingID, p.ExternalUserID)
		if e != nil {
			return out, resp.InvalidExternalUserIDs, e
		}
		if p.Nickname != "" {
			if e = store.SetNickname(ctx, cust.ID, p.Nickname, time.Now()); e != nil {
				return out, resp.InvalidExternalUserIDs, e
			}
			cust, e = store.Customer(ctx, cust.ID)
			if e != nil {
				return out, resp.InvalidExternalUserIDs, e
			}
		}
		out = append(out, cust)
	}
	return out, resp.InvalidExternalUserIDs, nil
}
