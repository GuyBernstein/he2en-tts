package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var vlog = func(string, ...any) {}

// ── Config ───────────────────────────────────────────────────────────

type VoiceProfile struct {
	VoiceID       string                 `json:"voice_id,omitempty"`
	VoiceName     string                 `json:"voice_name,omitempty"`
	Emotion       string                 `json:"emotion,omitempty"`
	VoiceSettings map[string]interface{} `json:"voice_settings,omitempty"`
}

type ProviderConfig struct {
	Enabled   bool   `json:"enabled"`
	APIKeyEnv string `json:"api_key_env"`
	apiKey    string
	ModelID   string       `json:"model_id,omitempty"`
	Language  string       `json:"language,omitempty"`
	Morning   VoiceProfile `json:"morning"`
	Evening   VoiceProfile `json:"evening"`
}

type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
	Order     []string                  `json:"order"`
	Timezone  string                    `json:"timezone"`
	Budget    BudgetConfig              `json:"budget"`
}

type BudgetConfig struct {
	ElevenLabs int `json:"elevenlabs"`
	Cartesia   int `json:"cartesia"`
	Chirp3HD   int `json:"chirp3_hd"`
	Standard   int `json:"standard"`
}

// ── Time-of-day ──────────────────────────────────────────────────────

func isEvening(tz string) bool {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, _ = time.LoadLocation("Asia/Jerusalem")
	}
	hour := time.Now().In(loc).Hour()
	return hour >= 17 || hour < 5
}

func periodLabel(evening bool) string {
	if evening {
		return "evening"
	}
	return "morning"
}

func resolveProfile(p ProviderConfig, evening bool) VoiceProfile {
	if evening {
		return p.Evening
	}
	return p.Morning
}

// ── Budget tracker ───────────────────────────────────────────────────

type BudgetData struct {
	Month       string         `json:"month"`
	Tiers       map[string]int `json:"tiers"`
	LastUpdated string         `json:"last_updated"`
}

type Budget struct {
	mu     sync.Mutex
	path   string
	data   BudgetData
	limits map[string]int
}

func NewBudget(path string, limits map[string]int) *Budget {
	b := &Budget{path: path, limits: limits}
	b.load()
	return b
}

func (b *Budget) currentMonth() string {
	return time.Now().UTC().Format("2006-01")
}

func (b *Budget) load() {
	b.mu.Lock()
	defer b.mu.Unlock()

	month := b.currentMonth()
	raw, err := os.ReadFile(b.path)
	if err == nil {
		if err := json.Unmarshal(raw, &b.data); err == nil {
			if b.data.Month == month && b.data.Tiers != nil {
				return
			}
		}
	}
	b.data = BudgetData{Month: month, Tiers: map[string]int{}}
	b.save()
}

func (b *Budget) save() {
	b.data.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	os.MkdirAll(filepath.Dir(b.path), 0755)
	raw, _ := json.MarshalIndent(b.data, "", "  ")
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err == nil {
		os.Rename(tmp, b.path)
	}
}

func (b *Budget) CanSpend(tier string, chars int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data.Month != b.currentMonth() {
		b.data = BudgetData{Month: b.currentMonth(), Tiers: map[string]int{}}
	}
	limit, ok := b.limits[tier]
	if !ok {
		return false
	}
	return (b.data.Tiers[tier] + chars) <= limit
}

func (b *Budget) Record(tier string, chars int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data.Tiers == nil {
		b.data.Tiers = map[string]int{}
	}
	b.data.Tiers[tier] += chars
	b.save()
}

func (b *Budget) Summary() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "month: %s\n", b.data.Month)
	for tier, limit := range b.limits {
		used := b.data.Tiers[tier]
		fmt.Fprintf(&sb, "  %-12s %9d / %9d (%5.1f%%)\n", tier, used, limit, float64(used)/float64(limit)*100)
	}
	return sb.String()
}

// ── WAV header ───────────────────────────────────────────────────────

func wrapWAVHeader(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	dataLen := len(pcm)
	byteRate := sampleRate * channels * (bitsPerSample / 8)
	blockAlign := channels * (bitsPerSample / 8)
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], uint16(bitsPerSample))
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	copy(buf[44:], pcm)
	return buf
}

// ── Provider implementations ─────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

func ttsElevenLabs(cfg ProviderConfig, vp VoiceProfile, text string) ([]byte, error) {
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s?output_format=pcm_48000", vp.VoiceID)

	payload := map[string]interface{}{
		"text":     text,
		"model_id": cfg.ModelID,
	}
	if cfg.Language != "" {
		payload["language_code"] = cfg.Language
	}
	if len(vp.VoiceSettings) > 0 {
		payload["voice_settings"] = vp.VoiceSettings
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", cfg.apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elevenlabs %d: %s", resp.StatusCode, truncate(b, 200))
	}

	pcm, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs read: %w", err)
	}
	return wrapWAVHeader(pcm, 48000, 1, 16), nil
}

func ttsCartesia(cfg ProviderConfig, vp VoiceProfile, text string) ([]byte, error) {
	url := "https://api.cartesia.ai/tts/bytes"

	payload := map[string]interface{}{
		"model_id":   cfg.ModelID,
		"transcript": text,
		"voice": map[string]interface{}{
			"mode": "id",
			"id":   vp.VoiceID,
		},
		"language": cfg.Language,
		"output_format": map[string]interface{}{
			"container":   "wav",
			"encoding":    "pcm_s16le",
			"sample_rate": 48000,
		},
	}
	if vp.Emotion != "" {
		payload["generation_config"] = map[string]string{"emotion": vp.Emotion}
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cartesia-Version", "2025-11-04")
	req.Header.Set("X-API-Key", cfg.apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cartesia request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cartesia %d: %s", resp.StatusCode, truncate(b, 200))
	}

	return io.ReadAll(resp.Body)
}

func ttsGoogle(cfg ProviderConfig, vp VoiceProfile, text string) ([]byte, error) {
	url := fmt.Sprintf("https://texttospeech.googleapis.com/v1/text:synthesize?key=%s", cfg.apiKey)

	languageCode := cfg.Language
	if languageCode == "" {
		languageCode = "en-US"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"input": map[string]string{"text": text},
		"voice": map[string]string{
			"languageCode": languageCode,
			"name":         vp.VoiceName,
		},
		"audioConfig": map[string]interface{}{
			"audioEncoding":  "LINEAR16",
			"sampleRateHertz": 48000,
		},
	})

	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google tts request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("google tts %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	var result struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("google tts decode: %w", err)
	}
	if result.AudioContent == "" {
		return nil, fmt.Errorf("google tts: empty audioContent")
	}

	return base64.StdEncoding.DecodeString(result.AudioContent)
}

// ── Orchestrator ─────────────────────────────────────────────────────

type providerFunc func(ProviderConfig, VoiceProfile, string) ([]byte, error)

var providers = map[string]providerFunc{
	"elevenlabs":    ttsElevenLabs,
	"cartesia":      ttsCartesia,
	"google_chirp3": ttsGoogle,
	"google_std":    ttsGoogle,
}

// budgetTier maps provider -> tier key for usage TRACKING (all providers, for dashboard)
var budgetTier = map[string]string{
	"elevenlabs":    "elevenlabs",
	"cartesia":      "cartesia",
	"google_chirp3": "chirp3_hd",
	"google_std":    "standard",
}

// budgetGated maps provider -> tier key for client-side ENFORCEMENT (Google only).
// Google starts billing beyond the free tier, so we must gate on our side.
// ElevenLabs/Cartesia enforce limits server-side via 429; we just track usage.
var budgetGated = map[string]string{
	"google_chirp3": "chirp3_hd",
	"google_std":    "standard",
}

func synthesize(cfg *Config, budget *Budget, text string, evening bool, report bool) ([]byte, string, error) {
	chars := len([]rune(text))
	period := periodLabel(evening)

	for _, name := range cfg.Order {
		pcfg, ok := cfg.Providers[name]
		if !ok || !pcfg.Enabled || pcfg.apiKey == "" {
			continue
		}

		fn, ok := providers[name]
		if !ok {
			continue
		}

		if tier, gated := budgetGated[name]; gated {
			if !budget.CanSpend(tier, chars) {
				vlog("[tts] %s: budget exhausted, skipping", name)
				continue
			}
		}

		vp := resolveProfile(pcfg, evening)
		vlog("[tts] trying %s (%s) ...", name, period)
		start := time.Now()
		audio, err := fn(pcfg, vp, text)
		latencyMs := time.Since(start).Milliseconds()
		if err != nil {
			vlog("[tts] %s failed: %v", name, err)
			if report {
				go reportTTSEvent(proxyTTSEvent{
					Provider: name, Period: period, Success: false,
					LatencyMs: latencyMs, Chars: chars, Error: err.Error(),
				})
			}
			continue
		}

		if tier, tracked := budgetTier[name]; tracked {
			budget.Record(tier, chars)
		}

		vlog("[tts] %s OK (%d bytes, %s profile)", name, len(audio), period)
		if report {
			go reportTTSEvent(proxyTTSEvent{
				Provider: name, Period: period, Success: true,
				LatencyMs: latencyMs, Chars: chars, Bytes: len(audio),
			})
		}
		return audio, name, nil
	}

	if report {
		go reportTTSEvent(proxyTTSEvent{
			Provider: "", Period: period, Success: false,
			Error: "all providers failed",
		})
	}
	return nil, "", fmt.Errorf("all TTS providers failed or budget-exhausted")
}

// ── Proxy event reporting (fire-and-forget) ─────────────────────────

var proxyEventURL = "http://127.0.0.1:4000/tts-event"
var eventClient = &http.Client{Timeout: 2 * time.Second}

type proxyTTSEvent struct {
	Provider  string `json:"provider"`
	Period    string `json:"period"`
	Success   bool   `json:"success"`
	LatencyMs int64  `json:"latency_ms"`
	Chars     int    `json:"chars"`
	Bytes     int    `json:"bytes"`
	Error     string `json:"error,omitempty"`
}

func reportTTSEvent(ev proxyTTSEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	resp, err := eventClient.Post(proxyEventURL, "application/json", bytes.NewReader(body))
	if err != nil {
		vlog("[tts] proxy event report failed (proxy down?): %v", err)
		return
	}
	resp.Body.Close()
	vlog("[tts] reported event to proxy: %s %s %v", ev.Provider, ev.Period, ev.Success)
}

// ── Test-all ─────────────────────────────────────────────────────────

func testAllProviders(cfg *Config, budget *Budget, tz string) {
	testText := "בדיקת קול"
	autoEvening := isEvening(tz)
	loc, _ := time.LoadLocation(tz)
	now := time.Now().In(loc)
	fmt.Printf("current time: %s (%s)\n", now.Format("15:04"), tz)
	fmt.Printf("auto-detected period: %s\n\n", periodLabel(autoEvening))

	periods := []struct {
		name    string
		evening bool
	}{
		{"morning", false},
		{"evening", true},
	}

	type result struct {
		provider string
		period   string
		ok       bool
		detail   string
	}
	var results []result

	for _, pd := range periods {
		for _, name := range cfg.Order {
			pcfg, ok := cfg.Providers[name]
			if !ok {
				continue
			}
			if !pcfg.Enabled {
				results = append(results, result{name, pd.name, false, "disabled"})
				continue
			}
			if pcfg.apiKey == "" {
				results = append(results, result{name, pd.name, false, "no API key"})
				continue
			}
			fn, ok := providers[name]
			if !ok {
				results = append(results, result{name, pd.name, false, "no provider func"})
				continue
			}
			vp := resolveProfile(pcfg, pd.evening)
			audio, err := fn(pcfg, vp, testText)
			if err != nil {
				results = append(results, result{name, pd.name, false, err.Error()})
				continue
			}
			detail := fmt.Sprintf("%d bytes", len(audio))
			if vp.VoiceID != "" {
				detail += fmt.Sprintf(", voice=%s", vp.VoiceID[:8]+"...")
			}
			if vp.VoiceName != "" {
				detail += fmt.Sprintf(", voice=%s", vp.VoiceName)
			}
			if vp.Emotion != "" {
				detail += fmt.Sprintf(", emotion=%s", vp.Emotion)
			}
			if stab, ok := vp.VoiceSettings["stability"]; ok {
				detail += fmt.Sprintf(", stability=%.2f", stab)
			}
			if style, ok := vp.VoiceSettings["style"]; ok {
				detail += fmt.Sprintf(", style=%.2f", style)
			}
			results = append(results, result{name, pd.name, true, detail})
		}
	}

	passChar, failChar := "✓", "✗"
	for _, r := range results {
		status := failChar
		if r.ok {
			status = passChar
		}
		fmt.Printf("  %s  %-15s  %-8s  %s\n", status, r.provider, r.period, r.detail)
	}

	pass, fail := 0, 0
	for _, r := range results {
		if r.ok {
			pass++
		} else {
			fail++
		}
	}
	fmt.Printf("\n%d passed, %d failed out of %d tests\n", pass, fail, pass+fail)
}

// ── Helpers ──────────────────────────────────────────────────────────

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func defaultConfig() *Config {
	return &Config{
		Order:    []string{"google_chirp3", "google_std"},
		Timezone: "America/New_York",
		Providers: map[string]ProviderConfig{
			"google_chirp3": {
				Enabled:   true,
				APIKeyEnv: "GEMINI_API_KEY",
				Language:  "en-US",
				Morning:   VoiceProfile{VoiceName: "en-US-Chirp3-HD-Achird"},
				Evening:   VoiceProfile{VoiceName: "en-US-Chirp3-HD-Achird"},
			},
			"google_std": {
				Enabled:   true,
				APIKeyEnv: "GEMINI_API_KEY",
				Language:  "en-US",
				Morning:   VoiceProfile{VoiceName: "en-US-Standard-B"},
				Evening:   VoiceProfile{VoiceName: "en-US-Standard-A"},
			},
		},
		Budget: BudgetConfig{
			Chirp3HD: 900_000,
			Standard: 3_600_000,
		},
	}
}

// ── Env loader (same pattern as picoclaw-proxy) ──────────────────────

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	for name, p := range cfg.Providers {
		if p.APIKeyEnv != "" {
			p.apiKey = os.Getenv(p.APIKeyEnv)
		}
		cfg.Providers[name] = p
	}
	return &cfg, nil
}

// ── Main ─────────────────────────────────────────────────────────────

func main() {
	text := flag.String("text", "", "Hebrew text to synthesize (required)")
	output := flag.String("output", "picoclaw_tts_output.wav", "Output WAV path (48kHz mono S16LE)")
	configPath := flag.String("config", "", "Config JSON path (optional, uses defaults)")
	defaultEnv := filepath.Join(os.Getenv("HOME"), ".picoclaw", "proxy", "env.conf")
	envPath := flag.String("env", defaultEnv, "Env file path")
	showBudget := flag.Bool("budget", false, "Print budget summary and exit")
	forcePeriod := flag.String("period", "", "Force time period: morning or evening (auto-detect by default)")
	testAll := flag.Bool("test", false, "Test all providers for both periods (morning+evening) and report results")
	verbose := flag.Bool("verbose", false, "Print provider logs to stderr")
	flag.Parse()

	if *verbose {
		vlog = log.Printf
	}

	if *text == "" && flag.NArg() > 0 {
		joined := strings.Join(flag.Args(), " ")
		text = &joined
	}

	loadEnvFile(*envPath)

	var cfg *Config
	if *configPath != "" {
		var err error
		cfg, err = loadConfig(*configPath)
		if err != nil {
			log.Fatalf("config error: %v", err)
		}
	} else {
		cfg = defaultConfig()
	}

	for name, p := range cfg.Providers {
		if p.apiKey == "" && p.APIKeyEnv != "" {
			p.apiKey = os.Getenv(p.APIKeyEnv)
		}
		cfg.Providers[name] = p
	}

	budgetPath := filepath.Join(os.Getenv("HOME"), ".picoclaw", "tts_usage.json")
	budget := NewBudget(budgetPath, map[string]int{
		"elevenlabs": cfg.Budget.ElevenLabs,
		"cartesia":   cfg.Budget.Cartesia,
		"chirp3_hd":  cfg.Budget.Chirp3HD,
		"standard":   cfg.Budget.Standard,
	})

	if *showBudget {
		fmt.Print(budget.Summary())
		os.Exit(0)
	}

	tz := cfg.Timezone
	if tz == "" {
		tz = "Asia/Jerusalem"
	}

	if *testAll {
		testAllProviders(cfg, budget, tz)
		os.Exit(0)
	}

	if *text == "" {
		fmt.Fprintln(os.Stderr, "usage: picoclaw-tts --text 'שלום' [--output out.wav] [--period morning|evening] [--test]")
		os.Exit(1)
	}

	evening := isEvening(tz)
	switch *forcePeriod {
	case "morning":
		evening = false
	case "evening":
		evening = true
	}

	vlog("[tts] period: %s (tz: %s)", periodLabel(evening), tz)

	audio, provider, err := synthesize(cfg, budget, *text, evening, true)
	if err != nil {
		log.Fatalf("tts failed: %v", err)
	}

	if dir := filepath.Dir(*output); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("create output dir: %v", err)
		}
	}
	if err := os.WriteFile(*output, audio, 0644); err != nil {
		log.Fatalf("write output: %v", err)
	}

	fmt.Printf("%s\n", *output)
	vlog("wrote %d bytes via %s (%s) → %s", len(audio), provider, periodLabel(evening), *output)

	// Brief pause for fire-and-forget proxy event reporting goroutine
	time.Sleep(100 * time.Millisecond)
}
