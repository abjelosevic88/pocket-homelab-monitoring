package sdk

import (
	"context"
	"net/http"
)

// PushMessage is a notification for a person's Pocket Hub devices (Mac, iPhone, iPad).
// Core clips Title to 120 and Body to 900 characters and seals it per device; see docs/push.md.
type PushMessage struct {
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"` // opened inside Pocket Hub when the notification is clicked
	Source string `json:"source,omitempty"`
	ID     string `json:"id,omitempty"`
	Sound  bool   `json:"sound"`
	Thread string `json:"thread,omitempty"` // groups notifications (e.g. the source)
}

// PushResult is how a notification went. Devices 0 means the person has no Pocket Hub with
// notifications on, which is not an error.
type PushResult struct {
	Devices int    `json:"devices"`
	Sent    int    `json:"sent"`
	Failed  int    `json:"failed"`
	Gone    int    `json:"gone"`
	Reason  string `json:"reason,omitempty"`
}

// Notify sends msg to every Pocket Hub device of the person with this email (POST
// /api/v1/push/notify with the app token). Core refuses people outside the app's audience
// (403); a 502/503 means the push relay was unreachable or over its limit and may be retried.
func (c *Client) Notify(ctx context.Context, email string, msg PushMessage) (*PushResult, error) {
	body := struct {
		Email string `json:"email"`
		PushMessage
	}{email, msg}
	var out PushResult
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/push/notify", c.Token(), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
