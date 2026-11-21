package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rakyll/ax-swarm/internal/model"
	"github.com/rakyll/ax-swarm/internal/store"
)

// Client talks to ax-swarm-server over HTTP/JSON.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{},
	}
}

func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

func (c *Client) CreateSwarm(ctx context.Context, p store.CreateSwarmParams) (model.Swarm, error) {
	var out model.Swarm
	err := c.do(ctx, http.MethodPost, "/v1/swarms", map[string]string{
		"swarm_id":      p.SwarmID,
		"atespace":      p.Atespace,
		"title":         p.Title,
		"workspace_ref": p.WorkspaceRef,
	}, &out)
	return out, err
}

func (c *Client) GetSwarm(ctx context.Context, swarmID string) (model.Swarm, error) {
	var out model.Swarm
	err := c.do(ctx, http.MethodGet, "/v1/swarms/"+url.PathEscape(swarmID), nil, &out)
	return out, err
}

func (c *Client) SetTitle(ctx context.Context, swarmID, title string) (model.Swarm, error) {
	var out model.Swarm
	err := c.do(ctx, http.MethodPut, "/v1/swarms/"+url.PathEscape(swarmID)+"/title", map[string]string{"title": title}, &out)
	return out, err
}

type AddMemberResponse struct {
	Member       model.Member  `json:"member"`
	Token        string        `json:"token"`
	BriefMessage model.Message `json:"brief_message"`
}

func (c *Client) AddMember(ctx context.Context, p store.AddMemberParams) (AddMemberResponse, error) {
	var out AddMemberResponse
	err := c.do(ctx, http.MethodPost, "/v1/swarms/"+url.PathEscape(p.SwarmID)+"/members", map[string]any{
		"name":        p.Name,
		"role":        p.Role,
		"model":       p.Model,
		"runtime":     p.Runtime,
		"image":       p.Image,
		"workspace":   p.Workspace,
		"brief":       p.Brief,
		"channels":    p.Channels,
		"node_id":     p.NodeID,
		"owner_kind":  p.OwnerKind,
		"owner_label": p.OwnerLabel,
		"posted_by":   p.PostedBy,
	}, &out)
	return out, err
}

func (c *Client) KillMember(ctx context.Context, swarmID, name string) (model.Member, error) {
	var out model.Member
	err := c.do(ctx, http.MethodDelete, "/v1/swarms/"+url.PathEscape(swarmID)+"/members/"+url.PathEscape(name), nil, &out)
	return out, err
}

type PostResponse struct {
	Message model.Message `json:"message"`
	Woken   []string      `json:"woken"`
}

func (c *Client) PostMessage(ctx context.Context, p store.PostMessageParams) (PostResponse, error) {
	var out PostResponse
	err := c.do(ctx, http.MethodPost, "/v1/swarms/"+url.PathEscape(p.SwarmID)+"/messages", map[string]string{
		"channel": p.Channel,
		"agent":   p.Agent,
		"text":    p.Text,
	}, &out)
	return out, err
}

type ReadResponse struct {
	Head     int64           `json:"head"`
	Messages []model.Message `json:"messages"`
}

func (c *Client) ReadBoard(ctx context.Context, swarmID, member string, since int64, channel string) (ReadResponse, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if member != "" {
		q.Set("member", member)
	}
	if channel != "" {
		q.Set("channel", channel)
	}
	path := "/v1/swarms/" + url.PathEscape(swarmID) + "/messages"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out ReadResponse
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) ParkMember(ctx context.Context, p store.ParkMemberParams) (store.ParkResult, error) {
	var out store.ParkResult
	path := "/v1/swarms/" + url.PathEscape(p.SwarmID) + "/members/" + url.PathEscape(p.Name) + "/park"
	err := c.do(ctx, http.MethodPost, path, map[string]any{
		"since":     p.Since,
		"wake_mode": p.WakeMode,
	}, &out)
	return out, err
}

// Watch streams NDJSON WatchSnapshots from ax-swarm-server until ctx is cancelled.
func (c *Client) Watch(ctx context.Context, swarmID string, onSnap func(model.WatchSnapshot) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/swarms/"+url.PathEscape(swarmID)+"/watch", nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var snap model.WatchSnapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			return err
		}
		if err := onSnap(snap); err != nil {
			return err
		}
	}
	return sc.Err()
}
