package store

// User is an account. Data is strictly per person; other people see only
// the servers shared with them.
type User struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Role         string `json:"role"` // admin | member
	FeedToken    string `json:"-"`
	CreatedAt    string `json:"created_at"`
}

// APIToken is a personal access token (hash stored, plaintext shown once).
type APIToken struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	LastUsedAt *string `json:"last_used_at"`
	CreatedAt  string  `json:"created_at"`
}

// Person is the public face of an account (for sharing).
type Person struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Server is one monitored machine.
type Server struct {
	ID        int64   `json:"id"`
	UserID    int64   `json:"owner_id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"` // local | agent | glances
	URL       string  `json:"url"`
	Username  string  `json:"username"`
	Secret    string  `json:"-"` // sealed
	Color     string  `json:"color"`
	Enabled   bool    `json:"enabled"`
	IntervalS int     `json:"interval_s"`
	SortOrder float64 `json:"sort_order"`
	LastSeen  string  `json:"last_seen"`
	LastError string  `json:"last_error"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	HasSecret bool    `json:"has_secret"`
}

// Share gives someone (or everyone) read access to a server.
type Share struct {
	ID        int64   `json:"id"`
	ServerID  int64   `json:"server_id"`
	UserID    *int64  `json:"user_id"`
	Everyone  bool    `json:"everyone"`
	Person    *Person `json:"person,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// Rule is one alert condition on a server.
type Rule struct {
	ID        int64   `json:"id"`
	ServerID  int64   `json:"server_id"`
	Metric    string  `json:"metric"`
	Target    string  `json:"target"`
	Threshold float64 `json:"threshold"`
	ForS      int     `json:"for_s"`
	Severity  string  `json:"severity"`
	Enabled   bool    `json:"enabled"`
	CreatedAt string  `json:"created_at"`
}

// Alert is one firing of a rule.
type Alert struct {
	ID         int64   `json:"id"`
	ServerID   int64   `json:"server_id"`
	ServerName string  `json:"server_name"`
	RuleID     *int64  `json:"rule_id"`
	Metric     string  `json:"metric"`
	Target     string  `json:"target"`
	Severity   string  `json:"severity"`
	Message    string  `json:"message"`
	Value      float64 `json:"value"`
	Threshold  float64 `json:"threshold"`
	StartedAt  string  `json:"started_at"`
	ResolvedAt *string `json:"resolved_at"`
}

// Sample is one point of history.
type Sample struct {
	T      int64              `json:"t"`
	CPU    float64            `json:"cpu"`
	IOWait float64            `json:"iowait"`
	Mem    float64            `json:"mem"`
	Swap   float64            `json:"swap"`
	Load1  float64            `json:"load1"`
	Load5  float64            `json:"load5"`
	Load15 float64            `json:"load15"`
	Rx     float64            `json:"rx"`
	Tx     float64            `json:"tx"`
	Read   float64            `json:"read"`
	Write  float64            `json:"write"`
	Temp   float64            `json:"temp"`
	Temps  map[string]float64 `json:"temps,omitempty"`
}

// SensorName is one sensor seen on a server.
type SensorName struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}
