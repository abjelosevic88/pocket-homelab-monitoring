// Shapes of the JSON API (see docs/api.md). Go: internal/collect/types.go, internal/store/models.go.

export interface User {
  id: number
  email: string
  name: string
  username: string
  role: 'admin' | 'member'
  created_at: string
}

export interface AuthStatus {
  needs_setup: boolean
  core_configured?: boolean
  auth_mode: 'local' | 'proxy' | 'none'
  authenticated: boolean
  user: User | null
  version: string
  title: string
  oidc?: boolean
  oidc_name?: string
}

export type RangeName = 'live' | '1h' | '6h' | '24h' | '7d' | '30d' | '90d'

export interface Settings {
  title: string
  setup_complete: boolean
  time_format: '24h' | '12h'
  default_range: RangeName
  density: 'comfortable' | 'compact'
  temp_unit: 'C' | 'F'
  notify_kind: 'apprise' | 'ntfy' | 'webhook'
  notify_url: string
  notify_alerts: boolean
  notify_resolved: boolean
}

export interface SettingsResponse {
  settings: Settings
  notify_has_token: boolean
  notify_box_route: boolean
  local_collector: boolean
  core: { configured: boolean; url: string; registered: boolean; error: string }
}

export interface Person { id: number; name: string; email: string }

export interface Share {
  id: number
  server_id: number
  user_id: number | null
  everyone: boolean
  person?: Person
  created_at: string
}

export type ServerKind = 'local' | 'agent' | 'glances'
export type ServerStatus = 'up' | 'down' | 'unknown' | 'paused'

/** The headline numbers of one server's latest snapshot. */
export interface Summary {
  time: string
  hostname: string
  platform: string
  cpu: number
  mem_pct: number
  mem_used: number
  mem_total: number
  swap_pct: number
  load1: number
  cores: number
  threads: number
  temp: number
  temp_label: string
  fs_pct: number
  fs_mount: string
  rx_bps: number
  tx_bps: number
  read_bps: number
  write_bps: number
  uptime_s: number
  procs: number
  containers_running: number
  containers_total: number
  containers_unhealthy: number
}

export interface Server {
  id: number
  name: string
  kind: ServerKind
  url: string
  username: string
  color: string
  enabled: boolean
  interval_s: number
  sort_order: number
  has_secret: boolean
  created_at: string
  role: 'owner' | 'viewer'
  owner?: Person
  shares?: Share[]
  status: ServerStatus
  last_seen: string
  last_error: string
  summary: Summary | null
  /** Last hour of CPU %, one value per minute (oldest first) for sparklines. */
  spark: number[]
  alerts_active: number
}

// ---- snapshot (internal/collect/types.go) ----

export interface Host {
  hostname: string; os: string; platform: string; kernel: string; arch: string; virtualization?: string
  cpu_model: string; cores: number; threads: number; boot_time: number; uptime_s: number; ips: string[]; agent_version?: string
}
export interface CPU {
  total: number; user: number; system: number; nice: number; iowait: number; irq: number; softirq: number; steal: number; idle: number
  per_core: number[]; freq_mhz: number; ctx_switches_s: number; interrupts_s: number
}
export interface Load { one: number; five: number; fifteen: number }
export interface Mem { total: number; used: number; available: number; free: number; buffers: number; cached: number; shared: number; percent: number }
export interface Swap { total: number; used: number; percent: number; in_bps: number; out_bps: number }
export interface NetIf {
  name: string; rx_bps: number; tx_bps: number; rx_total: number; tx_total: number; rx_errors: number; tx_errors: number; drops: number
  up: boolean; speed_mbps: number; mtu: number; addrs: string[]; virtual: boolean
}
export interface DiskIO { name: string; read_bps: number; write_bps: number; read_iops: number; write_iops: number; busy: number; read_total: number; write_total: number }
export interface FS { device: string; mount: string; type: string; total: number; used: number; free: number; percent: number; inodes_percent: number; read_only: boolean; network: boolean }
export interface Sensor { key: string; label: string; chip: string; kind: 'temp' | 'fan' | 'power' | 'voltage'; value: number; high?: number; critical?: number; unit: string }
export interface Proc {
  pid: number; ppid: number; name: string; user: string; cpu: number; mem_pct: number; rss: number; threads: number; state: string
  cmd: string; started: number; container?: string; read_bps: number; write_bps: number
}
export interface Procs { total: number; running: number; sleeping: number; zombie: number; threads: number; top: Proc[] }
export interface Container {
  id: string; name: string; image: string; state: string; status: string; health: string; project?: string
  cpu: number; mem_used: number; mem_limit: number; rx_bps: number; tx_bps: number; read_bps: number; write_bps: number
  started: number; restarts: number; ports?: string
}
export interface RAID { name: string; level: string; state: string; devices: number; active: number; degraded: boolean; sync?: number }

export interface Snapshot {
  time: string
  source: 'local' | 'agent' | 'glances'
  host: Host
  cpu: CPU
  load: Load
  mem: Mem
  swap: Swap
  net: NetIf[]
  disk_io: DiskIO[]
  fs: FS[]
  sensors: Sensor[]
  procs: Procs
  containers: Container[]
  raid: RAID[]
  containers_at: string
  errors?: string[]
}

// ---- container logs ----

/** One log line: t = RFC3339Nano from the engine, c = container name, s = out | err. */
export interface LogLine { t: string; c: string; s: 'out' | 'err'; m: string }
export interface Logs { lines: LogLine[]; truncated: boolean; now: string }

// ---- history ----

/** One point of history. t = unix seconds. Rates are bytes per second, temps °C. */
export interface Point {
  t: number
  cpu: number
  iowait: number
  mem: number
  swap: number
  load1: number
  load5: number
  load15: number
  rx: number
  tx: number
  read: number
  write: number
  temp: number
  /** Per-sensor temperatures by Sensor.key. */
  temps?: Record<string, number>
}

export interface History {
  range: RangeName
  step_s: number
  from: number
  to: number
  points: Point[]
  sensors: { key: string; label: string }[]
}

// ---- alerts ----

export type Metric = 'cpu' | 'mem' | 'swap' | 'load' | 'disk' | 'temp' | 'offline' | 'container' | 'raid' | 'job' | 'speedtest' | 'speed_down' | 'speed_up' | 'speed_ping'

// ---- internet speed tests ----

/** One speed test. Speeds in Mbps, times in ms; null = not measured. */
export interface SpeedTest {
  id: number
  server_id: number
  /** unix seconds */
  t: number
  ok: boolean
  error: string
  trigger: 'schedule' | 'manual' | 'recheck' | 'import'
  down_mbps: number | null
  up_mbps: number | null
  ping_ms: number | null
  jitter_ms: number | null
  loss_pct: number | null
  /** latency while the line is busy downloading / uploading (bufferbloat) */
  down_latency_ms: number | null
  up_latency_ms: number | null
  down_bytes: number
  up_bytes: number
  duration_ms: number
  isp: string
  external_ip: string
  vpn: boolean
  test_server_id: number
  test_server: string
  test_location: string
  result_url: string
  /** other traffic on the box right before the test */
  busy_mbps: number | null
  note: string
}

export interface SpeedSettings {
  enabled: boolean
  every_min: number
  offset_min: number
  test_server_id: number
  test_server: string
  plan_down: number
  plan_up: number
  min_down: number
  min_up: number
  max_ping: number
  busy_mbps: number
  keep_days: number
}

export interface SpeedLive {
  server_id: number
  trigger: string
  started: string
  phase: 'starting' | 'connecting' | 'ping' | 'download' | 'upload'
  progress: number
  mbps: number
  ping_ms: number
  down_mbps: number
  server?: OoklaServer
  isp?: string
  curve: { ms: number; phase: 'download' | 'upload'; mbps: number }[]
}

export interface SpeedInfo {
  available: boolean
  reason: string
  owner: boolean
  settings: SpeedSettings
  count: number
  latest?: SpeedTest
  live: SpeedLive | null
  next_at?: string
  recheck_at?: string
  put_off?: boolean
  busy_mbps: number
}

export interface OoklaServer { id: number; name: string; location: string; country: string; host: string }

// ---- scheduled jobs ----

export type JobStatus = 'ok' | 'running' | 'paused' | 'failed' | 'stuck' | 'overdue' | 'unscheduled'

export interface JobRun {
  unit: string
  invocation: string
  /** unix ms; 0 when unknown */
  start: number
  end: number
  ok: boolean
  result: string
  exit_status: number
  summary: string
  lines: number
  warnings: number
}

export interface Job {
  unit: string
  service: string
  scope: 'user' | 'system' | 'app'
  /** a Pocket app's own job (docs/jobs.md) */
  app?: string
  app_name?: string
  url?: string
  description: string
  schedule: string[] | null
  enabled: boolean
  file_state: string
  scheduled: boolean
  next: number
  last_trigger: number
  state: string
  result: string
  exit_status: number
  last_start: number
  last_end: number
  control: boolean
  name: string
  label: string
  group: string
  notes: string
  muted: boolean
  status: JobStatus
  problem: string
  week: { runs: number; failed: number; busy: number; avg_ms: number; max_ms: number }
  last_run: JobRun | null
  last_kept: JobRun | null
}

export interface JobsList {
  available: boolean
  reason: string
  error: string
  checked_at: string
  owner: boolean
  host?: string
  now?: number
  jobs: Job[]
  groups: string[]
}

export interface JobDay { day: string; runs: number; failed: number; busy: number }

export interface JobLogLine { t: number; p: number; m: string; i: string; sys?: boolean }

export interface Rule {
  id: number
  server_id: number
  metric: Metric
  /** Mount point, sensor key or container name; '' = any. */
  target: string
  threshold: number
  for_s: number
  severity: 'warning' | 'critical'
  enabled: boolean
  created_at: string
}

export interface Alert {
  id: number
  server_id: number
  server_name: string
  rule_id: number
  metric: Metric
  target: string
  severity: 'warning' | 'critical'
  message: string
  value: number
  threshold: number
  started_at: string
  resolved_at: string | null
}

export interface ApiToken { id: number; name: string; prefix: string; last_used_at: string | null; created_at: string }

/** GET /settings/system (About page). */
export interface SystemInfo {
  version: string
  go: string
  uptime: string
  auth_mode: string
  base_url: string
  timezone: string
  core: { configured: boolean; url: string; registered: boolean; error: string }
  counts: Record<string, number>
  db_size?: number
  users?: number
}
