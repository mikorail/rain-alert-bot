package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
)

func init() {
	// Load .env file if it exists (ignored in production if missing)
	_ = godotenv.Load()
}

// ============================================================================
// CONFIG
// ============================================================================

// Jakarta timezone used throughout the bot
var jakartaTZ = time.FixedZone("WIB", 7*3600)

type Config struct {
	TelegramToken string
	Port          string
	DefaultLat    float64
	DefaultLon    float64
}

func LoadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	lat := getEnvFloat("DEFAULT_LAT", -6.2088)
	lon := getEnvFloat("DEFAULT_LON", 106.8456)

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	// Basic token format validation (numeric_id:alphanumeric_secret)
	if !isValidTokenFormat(token) {
		log.Fatal("TELEGRAM_BOT_TOKEN has invalid format")
	}

	return Config{
		TelegramToken: token,
		Port:          port,
		DefaultLat:    lat,
		DefaultLon:    lon,
	}
}

func isValidTokenFormat(token string) bool {
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) == 0 || len(parts[1]) < 10 {
		return false
	}
	// First part should be numeric (bot ID)
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func getEnvFloat(key string, fallback float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		log.Printf("Warning: invalid float for %s=%q, using default %.4f", key, val, fallback)
		return fallback
	}
	return f
}

// ============================================================================
// OPEN-METEO TYPES
// ============================================================================

type OpenMeteoResponse struct {
	Hourly HourlyData `json:"hourly"`
}

type HourlyData struct {
	Time              []string  `json:"time"`
	WeatherCode       []int     `json:"weather_code"`
	Precipitation     []float64 `json:"precipitation"`
	Rain              []float64 `json:"rain"`
	Showers           []float64 `json:"showers"`
	PrecipProbability []int     `json:"precipitation_probability"`
	WindGusts10m      []float64 `json:"wind_gusts_10m"`
	Temperature2m     []float64 `json:"temperature_2m"`
	Humidity          []int     `json:"relative_humidity_2m"`
}

// WMO Weather interpretation codes
// https://open-meteo.com/en/docs
// 0: Clear sky
// 1,2,3: Mainly clear, partly cloudy, overcast
// 45,48: Fog / depositing rime fog
// 51,53,55: Drizzle (light, moderate, dense)
// 56,58: Freezing drizzle
// 61,63,65: Rain (slight, moderate, heavy)
// 66,67: Freezing rain
// 71,73,75: Snowfall (slight, moderate, heavy)
// 77: Snow grains
// 80,81,82: Rain showers (slight, moderate, violent)
// 85,86: Snow showers (slight, heavy)
// 95: Thunderstorm
// 96,99: Thunderstorm with hail

type WeatherAlert struct {
	Time        string
	Type        string
	Code        int
	Rain        float64
	Probability int
	WindGusts   float64
	Temperature float64
	Emoji       string
}

// ============================================================================
// SUBSCRIBER STORE (in-memory, file-backed)
// ============================================================================

type Subscriber struct {
	ChatID    int64   `json:"chat_id"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	CityName  string  `json:"city_name"`
	Active    bool    `json:"active"`
}

type SubscriberStore struct {
	mu          sync.RWMutex
	subscribers map[int64]*Subscriber
	filePath    string
}

func NewSubscriberStore(path string) *SubscriberStore {
	store := &SubscriberStore{
		subscribers: make(map[int64]*Subscriber),
		filePath:    path,
	}
	store.load()
	return store
}

func (s *SubscriberStore) load() {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		log.Printf("No existing subscribers file, starting fresh: %v", err)
		return
	}
	var subs []*Subscriber
	if err := json.Unmarshal(data, &subs); err != nil {
		log.Printf("Error parsing subscribers: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range subs {
		if sub.ChatID == 0 {
			continue // skip invalid entries
		}
		s.subscribers[sub.ChatID] = sub
	}
	log.Printf("Loaded %d subscribers", len(s.subscribers))
}

func (s *SubscriberStore) save() {
	s.mu.RLock()
	subs := make([]*Subscriber, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		subs = append(subs, sub)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		log.Printf("Error marshaling subscribers: %v", err)
		return
	}
	// Write to temp file first, then rename for atomic write
	tmpPath := s.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		log.Printf("Error writing temp subscribers file: %v", err)
		return
	}
	if err := os.Rename(tmpPath, s.filePath); err != nil {
		log.Printf("Error renaming subscribers file: %v", err)
	}
}

func (s *SubscriberStore) Add(sub *Subscriber) {
	s.mu.Lock()
	s.subscribers[sub.ChatID] = sub
	s.mu.Unlock()
	s.save()
}

func (s *SubscriberStore) Remove(chatID int64) {
	s.mu.Lock()
	delete(s.subscribers, chatID)
	s.mu.Unlock()
	s.save()
}

func (s *SubscriberStore) Get(chatID int64) (*Subscriber, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subscribers[chatID]
	return sub, ok
}

func (s *SubscriberStore) GetAll() []*Subscriber {
	s.mu.RLock()
	defer s.mu.RUnlock()
	subs := make([]*Subscriber, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		if sub.Active {
			subs = append(subs, sub)
		}
	}
	return subs
}

func (s *SubscriberStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subscribers)
}

// ============================================================================
// WEATHER FETCHER
// ============================================================================

// maxResponseSize limits API response body to 1MB to prevent memory exhaustion
const maxResponseSize = 1 * 1024 * 1024

func FetchWeather(lat, lon float64) (*OpenMeteoResponse, error) {
	// Validate coordinates
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, fmt.Errorf("invalid coordinates: lat=%.4f, lon=%.4f", lat, lon)
	}

	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f"+
			"&hourly=weather_code,precipitation,rain,showers,precipitation_probability,wind_gusts_10m,temperature_2m,relative_humidity_2m"+
			"&timezone=Asia%%2FJakarta&forecast_days=1",
		lat, lon,
	)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch weather: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-meteo returned status %d", resp.StatusCode)
	}

	// Limit response body size to prevent memory exhaustion
	limitedReader := io.LimitReader(resp.Body, maxResponseSize)
	var result OpenMeteoResponse
	if err := json.NewDecoder(limitedReader).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode weather: %w", err)
	}

	// Basic sanity check on response
	if len(result.Hourly.Time) == 0 {
		return nil, fmt.Errorf("empty weather data received")
	}

	return &result, nil
}

// ============================================================================
// ALERT ANALYSIS
// ============================================================================

func AnalyzeWeather(data *OpenMeteoResponse) []WeatherAlert {
	var alerts []WeatherAlert
	now := time.Now().In(jakartaTZ)

	for i, timeStr := range data.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", timeStr, jakartaTZ)
		if err != nil {
			continue
		}

		// Only look at upcoming hours (next 12 hours)
		if t.Before(now) || t.After(now.Add(12*time.Hour)) {
			continue
		}

		code := safeGetInt(data.Hourly.WeatherCode, i)
		rain := safeGetFloat(data.Hourly.Rain, i)
		prob := safeGetInt(data.Hourly.PrecipProbability, i)
		gusts := safeGetFloat(data.Hourly.WindGusts10m, i)
		temp := safeGetFloat(data.Hourly.Temperature2m, i)

		alert := classifyWeather(code, rain, prob, gusts)
		if alert != nil {
			alert.Time = t.Format("15:04")
			alert.Rain = rain
			alert.Probability = prob
			alert.WindGusts = gusts
			alert.Temperature = temp
			alerts = append(alerts, *alert)
		}
	}

	return deduplicateAlerts(alerts)
}

func safeGetInt(slice []int, idx int) int {
	if idx < len(slice) {
		return slice[idx]
	}
	return 0
}

func safeGetFloat(slice []float64, idx int) float64 {
	if idx < len(slice) {
		return slice[idx]
	}
	return 0
}

func classifyWeather(code int, rain float64, prob int, gusts float64) *WeatherAlert {
	switch {
	// Thunderstorm
	case code == 95 || code == 96 || code == 99:
		emoji := "⛈️"
		label := "Thunderstorm"
		if code == 96 || code == 99 {
			label = "Thunderstorm with Hail"
			emoji = "🌩️"
		}
		return &WeatherAlert{Type: label, Code: code, Emoji: emoji}

	// Heavy rain / violent showers
	case code == 65 || code == 82:
		return &WeatherAlert{Type: "Heavy Rain", Code: code, Emoji: "🌧️"}

	// Moderate rain / showers
	case code == 63 || code == 81:
		return &WeatherAlert{Type: "Moderate Rain", Code: code, Emoji: "🌦️"}

	// Light rain / slight showers
	case code == 61 || code == 80:
		return &WeatherAlert{Type: "Light Rain", Code: code, Emoji: "🌦️"}

	// Drizzle
	case code == 51 || code == 53 || code == 55:
		return &WeatherAlert{Type: "Drizzle", Code: code, Emoji: "🌧️"}

	// Freezing rain/drizzle
	case code == 56 || code == 57 || code == 66 || code == 67:
		return &WeatherAlert{Type: "Freezing Rain", Code: code, Emoji: "🧊"}

	// Snowfall
	case code == 71 || code == 73 || code == 75 || code == 77:
		return &WeatherAlert{Type: "Snowfall", Code: code, Emoji: "❄️"}

	// Snow showers
	case code == 85 || code == 86:
		return &WeatherAlert{Type: "Snow Showers", Code: code, Emoji: "🌨️"}

	// Fog
	case code == 45 || code == 48:
		return &WeatherAlert{Type: "Fog", Code: code, Emoji: "🌫️"}

	// High probability + high rain amount but code didn't catch it
	case prob >= 70 && rain > 0.5:
		return &WeatherAlert{Type: "Rain Expected", Code: code, Emoji: "☔"}

	// Strong wind gusts (tropical storm-like)
	case gusts >= 60:
		return &WeatherAlert{Type: "Strong Wind Gusts", Code: code, Emoji: "💨"}

	default:
		return nil
	}
}

// Group consecutive alerts of the same type into ranges
func deduplicateAlerts(alerts []WeatherAlert) []WeatherAlert {
	if len(alerts) == 0 {
		return alerts
	}

	var result []WeatherAlert
	current := alerts[0]
	endTime := current.Time

	for i := 1; i < len(alerts); i++ {
		if alerts[i].Type == current.Type {
			endTime = alerts[i].Time
			// Keep the worst stats
			if alerts[i].Rain > current.Rain {
				current.Rain = alerts[i].Rain
			}
			if alerts[i].WindGusts > current.WindGusts {
				current.WindGusts = alerts[i].WindGusts
			}
		} else {
			if endTime != current.Time {
				current.Time = current.Time + "–" + endTime
			}
			result = append(result, current)
			current = alerts[i]
			endTime = current.Time
		}
	}
	if endTime != current.Time {
		current.Time = current.Time + "–" + endTime
	}
	result = append(result, current)
	return result
}

// ============================================================================
// MESSAGE FORMATTING
// ============================================================================

func FormatAlertMessage(alerts []WeatherAlert, cityName string) string {
	if len(alerts) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🚨 *Weather Alert — %s*\n", escapeMarkdown(cityName)))
	sb.WriteString(fmt.Sprintf("📅 %s\n\n", escapeMarkdown(time.Now().In(jakartaTZ).Format("Mon, 02 Jan 2006"))))

	for _, a := range alerts {
		sb.WriteString(fmt.Sprintf(
			"%s *%s* at %s\n"+
				"   💧 Rain: %s mm \\| Prob: %d%%\n"+
				"   💨 Gusts: %s km/h \\| 🌡️ %s°C\n\n",
			a.Emoji, escapeMarkdown(a.Type), escapeMarkdown(a.Time),
			escapeMarkdown(fmt.Sprintf("%.1f", a.Rain)), a.Probability,
			escapeMarkdown(fmt.Sprintf("%.0f", a.WindGusts)), escapeMarkdown(fmt.Sprintf("%.1f", a.Temperature)),
		))
	}

	sb.WriteString("_Stay safe\\! Bring an umbrella_ ☂️")
	return sb.String()
}

func FormatCurrentWeather(data *OpenMeteoResponse, cityName string) string {
	if len(data.Hourly.Time) == 0 {
		return "No weather data available\\."
	}

	now := time.Now().In(jakartaTZ)
	bestIdx := 0
	for i, ts := range data.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", ts, jakartaTZ)
		if err != nil {
			continue
		}
		if !t.After(now) {
			bestIdx = i
		}
	}

	i := bestIdx
	var sb strings.Builder

	// Header with condition emoji
	condEmoji := "☁️"
	condDesc := "Unknown"
	if i < len(data.Hourly.WeatherCode) {
		condEmoji = wmoEmoji(data.Hourly.WeatherCode[i])
		condDesc = wmoDescription(data.Hourly.WeatherCode[i])
	}

	sb.WriteString(fmt.Sprintf("%s *%s*\n", condEmoji, escapeMarkdown(cityName)))
	sb.WriteString(fmt.Sprintf("_%s — %s WIB_\n", escapeMarkdown(condDesc), now.Format("15:04")))
	sb.WriteString("─────────────────\n")

	if i < len(data.Hourly.Temperature2m) {
		sb.WriteString(fmt.Sprintf("🌡️  %s°C", escapeMarkdown(fmt.Sprintf("%.1f", data.Hourly.Temperature2m[i]))))
	}
	if i < len(data.Hourly.Humidity) {
		sb.WriteString(fmt.Sprintf("   💦 %d%%", data.Hourly.Humidity[i]))
	}
	sb.WriteString("\n")
	if i < len(data.Hourly.Rain) {
		sb.WriteString(fmt.Sprintf("💧  %s mm", escapeMarkdown(fmt.Sprintf("%.1f", data.Hourly.Rain[i]))))
	}
	if i < len(data.Hourly.PrecipProbability) {
		sb.WriteString(fmt.Sprintf("   ☔ %d%%", data.Hourly.PrecipProbability[i]))
	}
	sb.WriteString("\n")
	if i < len(data.Hourly.WindGusts10m) {
		sb.WriteString(fmt.Sprintf("💨  %s km/h\n", escapeMarkdown(fmt.Sprintf("%.0f", data.Hourly.WindGusts10m[i]))))
	}

	return sb.String()
}

// FormatHourlyForecast creates a text-based hourly forecast from now until end of day
func FormatHourlyForecast(data *OpenMeteoResponse, cityName string) string {
	if len(data.Hourly.Time) == 0 {
		return "No forecast data available\\."
	}

	now := time.Now().In(jakartaTZ)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Hourly Forecast — %s*\n", escapeMarkdown(cityName)))
	sb.WriteString(fmt.Sprintf("_%s_\n", escapeMarkdown(now.Format("Mon, 02 Jan 2006"))))
	sb.WriteString("─────────────────\n")

	found := false
	for i, ts := range data.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", ts, jakartaTZ)
		if err != nil {
			continue
		}

		// Show from current hour through end of day
		if t.Before(now.Truncate(time.Hour)) {
			continue
		}
		// Stop at midnight (next day)
		if t.Day() != now.Day() {
			break
		}

		found = true
		temp := safeGetFloat(data.Hourly.Temperature2m, i)
		rain := safeGetFloat(data.Hourly.Rain, i)
		prob := safeGetInt(data.Hourly.PrecipProbability, i)
		gusts := safeGetFloat(data.Hourly.WindGusts10m, i)
		code := safeGetInt(data.Hourly.WeatherCode, i)

		emoji := wmoEmoji(code)
		hour := t.Format("15:04")

		// Mark current hour
		marker := ""
		if t.Hour() == now.Hour() {
			marker = " ◀️"
		}

		// Compact single-line per hour with rain bar indicator
		rainBar := ""
		if rain > 0 || prob >= 50 {
			rainBar = fmt.Sprintf(" 💧%s%%", escapeMarkdown(fmt.Sprintf("%d", prob)))
			if rain > 0 {
				rainBar += fmt.Sprintf("/%smm", escapeMarkdown(fmt.Sprintf("%.1f", rain)))
			}
		}

		gustInfo := ""
		if gusts >= 30 {
			gustInfo = fmt.Sprintf(" 💨%s", escapeMarkdown(fmt.Sprintf("%.0f", gusts)))
		}

		sb.WriteString(fmt.Sprintf(
			"`%s` %s %s°C%s%s%s\n",
			hour, emoji,
			escapeMarkdown(fmt.Sprintf("%.0f", temp)),
			rainBar, gustInfo, marker,
		))
	}

	if !found {
		sb.WriteString("_No more hours remaining today\\._\n")
	}

	// Add legend
	sb.WriteString("─────────────────\n")
	sb.WriteString("_💧prob/rain  💨gusts \\>30km/h_\n")

	return sb.String()
}

func wmoDescription(code int) string {
	switch code {
	case 0:
		return "Clear sky"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45:
		return "Fog"
	case 48:
		return "Depositing rime fog"
	case 51:
		return "Light drizzle"
	case 53:
		return "Moderate drizzle"
	case 55:
		return "Dense drizzle"
	case 56:
		return "Light freezing drizzle"
	case 57:
		return "Dense freezing drizzle"
	case 61:
		return "Slight rain"
	case 63:
		return "Moderate rain"
	case 65:
		return "Heavy rain"
	case 66:
		return "Light freezing rain"
	case 67:
		return "Heavy freezing rain"
	case 71:
		return "Slight snowfall"
	case 73:
		return "Moderate snowfall"
	case 75:
		return "Heavy snowfall"
	case 77:
		return "Snow grains"
	case 80:
		return "Slight rain showers"
	case 81:
		return "Moderate rain showers"
	case 82:
		return "Violent rain showers"
	case 85:
		return "Slight snow showers"
	case 86:
		return "Heavy snow showers"
	case 95:
		return "Thunderstorm"
	case 96:
		return "Thunderstorm + slight hail"
	case 99:
		return "Thunderstorm + heavy hail"
	default:
		return "Unknown"
	}
}

func wmoEmoji(code int) string {
	switch {
	case code == 0:
		return "☀️"
	case code >= 1 && code <= 2:
		return "⛅"
	case code == 3:
		return "☁️"
	case code == 45 || code == 48:
		return "🌫️"
	case code >= 51 && code <= 57:
		return "🌧️"
	case code >= 61 && code <= 67:
		return "🌧️"
	case code >= 71 && code <= 77:
		return "❄️"
	case code >= 80 && code <= 82:
		return "🌦️"
	case code >= 85 && code <= 86:
		return "🌨️"
	case code == 95:
		return "⛈️"
	case code == 96 || code == 99:
		return "🌩️"
	default:
		return "🌡️"
	}
}

func escapeMarkdown(s string) string {
	// MarkdownV2 requires escaping these characters: _ * [ ] ( ) ~ ` > # + - = | { } . !
	// We must escape backslash first to avoid double-escaping
	s = strings.ReplaceAll(s, "\\", "\\\\")
	for _, ch := range []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"} {
		s = strings.ReplaceAll(s, ch, "\\"+ch)
	}
	return s
}

// ============================================================================
// BOT HANDLER
// ============================================================================

// ============================================================================
// WEATHER SUMMARY (natural language)
// ============================================================================

// hourlySlot holds parsed data for one hour
type hourlySlot struct {
	Time     time.Time
	Code     int
	Rain     float64
	Prob     int
	Gusts    float64
	Temp     float64
	Humidity int
}

// parseUpcomingSlots extracts hourly data from now through end of day
func parseUpcomingSlots(data *OpenMeteoResponse) []hourlySlot {
	now := time.Now().In(jakartaTZ)
	var slots []hourlySlot

	for i, ts := range data.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", ts, jakartaTZ)
		if err != nil {
			continue
		}
		if t.Before(now.Truncate(time.Hour)) {
			continue
		}
		slots = append(slots, hourlySlot{
			Time:     t,
			Code:     safeGetInt(data.Hourly.WeatherCode, i),
			Rain:     safeGetFloat(data.Hourly.Rain, i),
			Prob:     safeGetInt(data.Hourly.PrecipProbability, i),
			Gusts:    safeGetFloat(data.Hourly.WindGusts10m, i),
			Temp:     safeGetFloat(data.Hourly.Temperature2m, i),
			Humidity: safeGetInt(data.Hourly.Humidity, i),
		})
	}
	return slots
}

func isRainyCode(code int) bool {
	// drizzle 51-57, rain 61-67, showers 80-82, thunderstorm 95-99
	return (code >= 51 && code <= 57) || (code >= 61 && code <= 67) ||
		(code >= 80 && code <= 82) || (code >= 95 && code <= 99)
}

func isSevereCode(code int) bool {
	return code == 65 || code == 82 || code == 95 || code == 96 || code == 99
}

func isClearCode(code int) bool {
	return code >= 0 && code <= 3
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// GenerateWeatherSummary produces a natural language summary like:
// "Rain for the next 2 hours, sunny window around 17:00. Take in laundry!"
func GenerateWeatherSummary(data *OpenMeteoResponse, cityName string) string {
	slots := parseUpcomingSlots(data)
	if len(slots) == 0 {
		return ""
	}

	now := time.Now().In(jakartaTZ)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🗣️ *Weather Summary — %s*\n", escapeMarkdown(cityName)))
	sb.WriteString(fmt.Sprintf("🕐 %s WIB\n\n", now.Format("15:04")))

	// Analyze weather phases: group consecutive hours by rain/clear
	type phase struct {
		startHour string
		endHour   string
		rainy     bool
		severe    bool
		maxRain   float64
		maxGusts  float64
		hours     int
	}

	var phases []phase
	if len(slots) > 0 {
		current := phase{
			startHour: slots[0].Time.Format("15:04"),
			endHour:   slots[0].Time.Format("15:04"),
			rainy:     isRainyCode(slots[0].Code),
			severe:    isSevereCode(slots[0].Code),
			maxRain:   slots[0].Rain,
			maxGusts:  slots[0].Gusts,
			hours:     1,
		}

		for i := 1; i < len(slots); i++ {
			slotRainy := isRainyCode(slots[i].Code)
			if slotRainy == current.rainy {
				current.endHour = slots[i].Time.Format("15:04")
				current.hours++
				if slots[i].Rain > current.maxRain {
					current.maxRain = slots[i].Rain
				}
				if slots[i].Gusts > current.maxGusts {
					current.maxGusts = slots[i].Gusts
				}
				if isSevereCode(slots[i].Code) {
					current.severe = true
				}
			} else {
				phases = append(phases, current)
				current = phase{
					startHour: slots[i].Time.Format("15:04"),
					endHour:   slots[i].Time.Format("15:04"),
					rainy:     slotRainy,
					severe:    isSevereCode(slots[i].Code),
					maxRain:   slots[i].Rain,
					maxGusts:  slots[i].Gusts,
					hours:     1,
				}
			}
		}
		phases = append(phases, current)
	}

	// Build natural language from phases
	hasRain := false
	hasClearWindow := false
	hasSevere := false

	for i, p := range phases {
		if p.rainy {
			hasRain = true
			if p.severe {
				hasSevere = true
			}

			intensity := "rain"
			if p.maxRain >= 5 {
				intensity = "heavy rain"
			} else if p.maxRain >= 1 {
				intensity = "moderate rain"
			} else if p.maxRain > 0 {
				intensity = "light rain"
			}

			if i == 0 {
				// Currently raining
				if p.hours == 1 {
					sb.WriteString(fmt.Sprintf("🌧️ %s right now until %s\\.\n",
						escapeMarkdown(capitalize(intensity)), escapeMarkdown(p.endHour)))
				} else {
					sb.WriteString(fmt.Sprintf("🌧️ %s for the next %d hours \\(until %s\\)\\.\n",
						escapeMarkdown(capitalize(intensity)), p.hours, escapeMarkdown(p.endHour)))
				}
			} else {
				// Rain coming later
				if p.hours == 1 {
					sb.WriteString(fmt.Sprintf("🌧️ %s expected around %s\\.\n",
						escapeMarkdown(capitalize(intensity)), escapeMarkdown(p.startHour)))
				} else {
					sb.WriteString(fmt.Sprintf("🌧️ %s from %s to %s \\(%d hours\\)\\.\n",
						escapeMarkdown(capitalize(intensity)),
						escapeMarkdown(p.startHour), escapeMarkdown(p.endHour), p.hours))
				}
			}

			if p.severe {
				sb.WriteString("⚠️ Severe conditions possible \\— stay indoors if you can\\.\n")
			}
			if p.maxGusts >= 40 {
				sb.WriteString(fmt.Sprintf("💨 Wind gusts up to %s km/h\\.\n",
					escapeMarkdown(fmt.Sprintf("%.0f", p.maxGusts))))
			}
		} else {
			hasClearWindow = true
			if i > 0 && phases[i-1].rainy {
				// Clear window after rain
				if p.hours >= 2 {
					sb.WriteString(fmt.Sprintf("☀️ Clearing up around %s — good window for %d hours\\.\n",
						escapeMarkdown(p.startHour), p.hours))
				} else {
					sb.WriteString(fmt.Sprintf("⛅ Brief clear window around %s\\.\n",
						escapeMarkdown(p.startHour)))
				}
			} else if i == 0 {
				// Currently clear
				sb.WriteString(fmt.Sprintf("☀️ Clear skies right now \\(until %s\\)\\.\n",
					escapeMarkdown(p.endHour)))
			}
		}
	}

	// Practical advice section
	sb.WriteString("\n")
	if hasRain {
		sb.WriteString("*What to do:*\n")
		if hasSevere {
			sb.WriteString("🏠 Postpone outdoor activities if possible\\.\n")
			sb.WriteString("⚡ Unplug sensitive electronics\\.\n")
		}
		sb.WriteString("👕 Take in the laundry/dryer\\!\n")
		sb.WriteString("☂️ Bring an umbrella if going out\\.\n")
		if hasClearWindow {
			// Find first clear window
			for _, p := range phases {
				if !p.rainy && p.hours >= 2 {
					sb.WriteString(fmt.Sprintf("🏃 Best outdoor window: around %s\\.\n",
						escapeMarkdown(p.startHour)))
					break
				}
			}
		}
	} else {
		sb.WriteString("✅ All clear\\! No rain expected\\. Good day to be outside\\.\n")
	}

	return sb.String()
}

// ============================================================================
// SUDDEN RAIN DETECTION
// ============================================================================

// SuddenRainTracker keeps track of last alert per subscriber to avoid spam
type SuddenRainTracker struct {
	mu        sync.RWMutex
	lastAlert map[int64]time.Time // chatID -> last sudden alert time
}

func NewSuddenRainTracker() *SuddenRainTracker {
	return &SuddenRainTracker{
		lastAlert: make(map[int64]time.Time),
	}
}

func (s *SuddenRainTracker) ShouldAlert(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	last, ok := s.lastAlert[chatID]
	if !ok {
		return true
	}
	// Don't re-alert within 2 hours for the same subscriber
	return time.Since(last) > 2*time.Hour
}

func (s *SuddenRainTracker) MarkAlerted(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAlert[chatID] = time.Now()
}

// DetectSuddenRain checks if rain/storm appears in the next 1-2 hours
// Returns true + urgency message if rain is imminent
func DetectSuddenRain(data *OpenMeteoResponse) (bool, string) {
	now := time.Now().In(jakartaTZ)

	for i, ts := range data.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", ts, jakartaTZ)
		if err != nil {
			continue
		}

		// Only check the next 2 hours
		if t.Before(now) || t.After(now.Add(2*time.Hour)) {
			continue
		}

		code := safeGetInt(data.Hourly.WeatherCode, i)
		rain := safeGetFloat(data.Hourly.Rain, i)
		prob := safeGetInt(data.Hourly.PrecipProbability, i)

		if !isRainyCode(code) && !(prob >= 70 && rain > 0.5) {
			continue
		}

		minutesAway := int(t.Sub(now).Minutes())
		if minutesAway < 0 {
			minutesAway = 0
		}

		urgency := ""
		if isSevereCode(code) {
			if minutesAway <= 30 {
				urgency = "Severe weather arriving NOW"
			} else {
				urgency = fmt.Sprintf("Severe weather in ~%d minutes", minutesAway)
			}
		} else {
			if minutesAway <= 30 {
				urgency = "Rain arriving soon"
			} else {
				urgency = fmt.Sprintf("Rain expected in ~%d minutes", minutesAway)
			}
		}

		return true, urgency
	}

	return false, ""
}

func FormatSuddenRainAlert(urgency, cityName string) string {
	var sb strings.Builder
	sb.WriteString("⚡ *SUDDEN RAIN ALERT*\n")
	sb.WriteString(fmt.Sprintf("📍 %s\n\n", escapeMarkdown(cityName)))
	sb.WriteString(fmt.Sprintf("🌧️ %s\\!\n\n", escapeMarkdown(urgency)))
	sb.WriteString("*Quick actions:*\n")
	sb.WriteString("👕 Take in the laundry NOW\\!\n")
	sb.WriteString("🪟 Close windows\\.\n")
	sb.WriteString("☂️ Grab an umbrella if heading out\\.\n")
	sb.WriteString("🚗 Drive carefully\\.\n")
	return sb.String()
}

// ============================================================================
// BOT HANDLER
// ============================================================================

type Bot struct {
	api          *tgbotapi.BotAPI
	store        *SubscriberStore
	config       Config
	suddenTracker *SuddenRainTracker
}

func NewBot(cfg Config) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	log.Printf("Authorized on account %s", api.Self.UserName)

	return &Bot{
		api:           api,
		store:         NewSubscriberStore("subscribers.json"),
		config:        cfg,
		suddenTracker: NewSuddenRainTracker(),
	}, nil
}

func (b *Bot) HandleUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	chatID := update.Message.Chat.ID

	// Handle location messages (user shares their location)
	if update.Message.Location != nil {
		loc := update.Message.Location
		// Validate coordinates
		if loc.Latitude < -90 || loc.Latitude > 90 || loc.Longitude < -180 || loc.Longitude > 180 {
			b.sendText(chatID, "⚠️ Invalid location coordinates\\. Please try again\\.")
			return
		}
		sub := &Subscriber{
			ChatID:    chatID,
			Latitude:  loc.Latitude,
			Longitude: loc.Longitude,
			CityName:  fmt.Sprintf("%.4f, %.4f", loc.Latitude, loc.Longitude),
			Active:    true,
		}
		b.store.Add(sub)
		b.sendText(chatID, "📍 Location saved\\! You'll now receive rain/storm alerts for your area\\.\n\nUse /check to see current weather or /forecast for hourly prediction\\.")
		return
	}

	if !update.Message.IsCommand() {
		return
	}

	switch update.Message.Command() {
	case "start":
		b.handleStart(chatID)
	case "subscribe":
		b.handleSubscribe(chatID)
	case "unsubscribe":
		b.handleUnsubscribe(chatID)
	case "check":
		b.handleCheck(chatID)
	case "forecast":
		b.handleForecast(chatID)
	case "status":
		b.handleStatus(chatID)
	case "help":
		b.handleHelp(chatID)
	default:
		b.sendText(chatID, "Unknown command\\. Use /help to see available commands\\.")
	}
}

func (b *Bot) sendText(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Error sending message to chat %d: %v", chatID, err)
	}
}

func (b *Bot) handleStart(chatID int64) {
	text := `🌧️ *Rain & Storm Alert Bot*

Selamat datang\! I'll notify you before it rains or storms near your location\.

*Quick Setup:*
1\. Send me your 📍 location \(tap the attachment icon\)
2\. Or use /subscribe to use default Jakarta coordinates

*Commands:*
/subscribe — Start receiving alerts
/unsubscribe — Stop alerts
/check — Check weather right now
/forecast — Hourly forecast till end of day
/status — Your subscription info
/help — Show this message

_Powered by Open\-Meteo API \(free, no API key needed\)_`

	b.sendText(chatID, text)
}

func (b *Bot) handleSubscribe(chatID int64) {
	if _, exists := b.store.Get(chatID); exists {
		b.sendText(chatID, "✅ You're already subscribed\\! Send a new 📍 location to update your area\\.")
		return
	}

	sub := &Subscriber{
		ChatID:    chatID,
		Latitude:  b.config.DefaultLat,
		Longitude: b.config.DefaultLon,
		CityName:  "Jakarta",
		Active:    true,
	}
	b.store.Add(sub)

	b.sendText(chatID, "✅ Subscribed with default location: *Jakarta*\\.\n\n📍 Send me your location to get alerts for your exact area\\!")
}

func (b *Bot) handleUnsubscribe(chatID int64) {
	b.store.Remove(chatID)
	b.sendText(chatID, "❌ Unsubscribed\\. You won't receive any more alerts\\.\n\nUse /subscribe to re\\-enable\\.")
}

func (b *Bot) handleCheck(chatID int64) {
	sub := b.getOrDefaultSubscriber(chatID)

	data, err := FetchWeather(sub.Latitude, sub.Longitude)
	if err != nil {
		log.Printf("Error fetching weather for check: %v", err)
		b.sendText(chatID, "⚠️ Couldn't fetch weather data\\. Try again in a moment\\.")
		return
	}

	// Current weather snapshot
	b.sendText(chatID, FormatCurrentWeather(data, sub.CityName))

	// Natural language summary with advice
	summary := GenerateWeatherSummary(data, sub.CityName)
	if summary != "" {
		b.sendText(chatID, summary)
	}
}

func (b *Bot) handleForecast(chatID int64) {
	sub := b.getOrDefaultSubscriber(chatID)

	data, err := FetchWeather(sub.Latitude, sub.Longitude)
	if err != nil {
		log.Printf("Error fetching weather for forecast: %v", err)
		b.sendText(chatID, "⚠️ Couldn't fetch forecast data\\. Try again in a moment\\.")
		return
	}

	b.sendText(chatID, FormatHourlyForecast(data, sub.CityName))
}

func (b *Bot) handleStatus(chatID int64) {
	sub, exists := b.store.Get(chatID)
	if !exists {
		b.sendText(chatID, "You're not subscribed\\. Use /subscribe or send your 📍 location\\.")
		return
	}
	text := fmt.Sprintf(
		"📊 *Your Subscription*\n\n"+
			"📍 Location: %s\n"+
			"🌐 Coordinates: %s, %s\n"+
			"✅ Active: %v\n"+
			"⏰ Alerts: Every 3 hours",
		escapeMarkdown(sub.CityName),
		escapeMarkdown(fmt.Sprintf("%.4f", sub.Latitude)),
		escapeMarkdown(fmt.Sprintf("%.4f", sub.Longitude)),
		sub.Active,
	)
	b.sendText(chatID, text)
}

func (b *Bot) handleHelp(chatID int64) {
	b.handleStart(chatID)
}

func (b *Bot) getOrDefaultSubscriber(chatID int64) *Subscriber {
	sub, exists := b.store.Get(chatID)
	if !exists {
		return &Subscriber{
			Latitude:  b.config.DefaultLat,
			Longitude: b.config.DefaultLon,
			CityName:  "Jakarta",
		}
	}
	return sub
}

// Scheduled alert check for all subscribers (every 3 hours)
func (b *Bot) CheckAllSubscribers() {
	subs := b.store.GetAll()
	log.Printf("Running scheduled check for %d subscribers", len(subs))

	for _, sub := range subs {
		data, err := FetchWeather(sub.Latitude, sub.Longitude)
		if err != nil {
			log.Printf("Error fetching weather for chat %d: %v", sub.ChatID, err)
			continue
		}

		// Send natural language summary (only if there's rain)
		summary := GenerateWeatherSummary(data, sub.CityName)
		if summary != "" {
			// Check if the summary mentions rain (skip all-clear for scheduled)
			alerts := AnalyzeWeather(data)
			if len(alerts) > 0 {
				b.sendText(sub.ChatID, summary)
			}
		}

		// Rate limit: don't spam Open-Meteo
		time.Sleep(500 * time.Millisecond)
	}
}

// SuddenRainCheck runs every 15 minutes — alerts subscribers
// immediately if rain is detected in the next 1-2 hours
func (b *Bot) SuddenRainCheck() {
	subs := b.store.GetAll()
	log.Printf("Running sudden rain check for %d subscribers", len(subs))

	for _, sub := range subs {
		if !b.suddenTracker.ShouldAlert(sub.ChatID) {
			continue
		}

		data, err := FetchWeather(sub.Latitude, sub.Longitude)
		if err != nil {
			log.Printf("Sudden rain check error for chat %d: %v", sub.ChatID, err)
			continue
		}

		detected, urgency := DetectSuddenRain(data)
		if !detected {
			continue
		}

		log.Printf("Sudden rain detected for chat %d: %s", sub.ChatID, urgency)
		b.sendText(sub.ChatID, FormatSuddenRainAlert(urgency, sub.CityName))
		b.suddenTracker.MarkAlerted(sub.ChatID)

		// Rate limit
		time.Sleep(500 * time.Millisecond)
	}
}

// ============================================================================
// MAIN
// ============================================================================

func main() {
	cfg := LoadConfig()

	bot, err := NewBot(cfg)
	if err != nil {
		log.Fatal(err)
	}

	// --- Cron scheduler ---
	c := cron.New(cron.WithLocation(jakartaTZ))

	// Regular summary every 3 hours
	_, err = c.AddFunc("0 5,8,11,14,17,20,23 * * *", func() {
		log.Println("Scheduled weather check triggered")
		bot.CheckAllSubscribers()
	})
	if err != nil {
		log.Fatalf("Failed to add scheduled check cron: %v", err)
	}

	// Sudden rain rapid check every 15 minutes
	_, err = c.AddFunc("*/15 * * * *", func() {
		bot.SuddenRainCheck()
	})
	if err != nil {
		log.Fatalf("Failed to add sudden rain cron: %v", err)
	}

	c.Start()
	log.Println("Cron started: scheduled alerts every 3h + sudden rain check every 15min")

	// --- Health check HTTP server (required for Render/Fly.io) ---
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Rain Alert Bot is running")
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	go func() {
		log.Printf("Health server listening on :%s", cfg.Port)
		if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
			log.Printf("Health server error: %v", err)
		}
	}()

	// --- Telegram long polling ---
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.api.GetUpdatesChan(u)

	log.Println("Bot is running. Waiting for messages...")

	for update := range updates {
		go bot.HandleUpdate(update)
	}
}
