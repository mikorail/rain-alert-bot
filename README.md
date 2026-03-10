# 🌧️ Rain & Storm Alert Bot (Telegram)

A Go-based Telegram bot that monitors weather near your location and sends automatic alerts for rain, storms, drizzle, and strong winds. Built for Jakarta but works anywhere.

## Features

- **Automatic alerts** every 3 hours (05:00, 08:00, 11:00, 14:00, 17:00, 20:00, 23:00 WIB)
- Detects: light rain, moderate rain, heavy rain, drizzle, thunderstorms, hail, strong wind gusts
- **Custom location** — share your GPS location or use default Jakarta coordinates
- **On-demand check** — `/check` to see current weather + upcoming alerts
- **Free everything** — Open-Meteo API (no key needed), free hosting on Render or Fly.io

## Weather Codes Detected

| Condition | WMO Codes | Alert |
|---|---|---|
| Drizzle | 51, 53, 55 | 🌧️ Drizzle |
| Light Rain | 61, 80 | 🌦️ Light Rain |
| Moderate Rain | 63, 81 | 🌦️ Moderate Rain |
| Heavy Rain | 65, 82 | 🌧️ Heavy Rain |
| Thunderstorm | 95 | ⛈️ Thunderstorm |
| Thunderstorm + Hail | 96, 99 | 🌩️ Thunderstorm with Hail |
| Strong Winds | gusts ≥60 km/h | 💨 Strong Wind Gusts |
| High Probability | prob ≥70% + rain >0.5mm | ☔ Rain Expected |

## Quick Start

### 1. Create a Telegram Bot

1. Open Telegram, search for **@BotFather**
2. Send `/newbot`
3. Follow the prompts — give it a name like "Rain Alert Jakarta"
4. Copy the API token (looks like `123456789:ABCdefGhIjKlMnOpQrS...`)

### 2. Run Locally

```bash
# Clone & enter
git clone https://github.com/YOUR_USERNAME/rain-alert-bot.git
cd rain-alert-bot

# Set your token
export TELEGRAM_BOT_TOKEN="your_token_here"

# Run
go mod tidy
go run main.go
```

### 3. Deploy to Render (Recommended — Free)

1. Push this repo to GitHub
2. Go to [render.com](https://render.com) → New → Web Service
3. Connect your GitHub repo
4. Render detects `render.yaml` automatically
5. Add environment variable: `TELEGRAM_BOT_TOKEN` = your token
6. Deploy!

> Render free tier keeps web services alive with the health check endpoint.
> The bot uses long polling so it doesn't need a webhook.

### 4. Deploy to Fly.io (Alternative — Free)

```bash
# Install flyctl
curl -L https://fly.io/install.sh | sh

# Login & deploy
fly auth login
fly launch          # say yes to using existing fly.toml
fly secrets set TELEGRAM_BOT_TOKEN="your_token_here"
fly vol create bot_data --region sin --size 1
fly deploy
```

> Fly.io gives you 3 free machines. Singapore region (`sin`) has lowest latency to Jakarta.

## Bot Commands

| Command | Description |
|---|---|
| `/start` | Welcome message + setup guide |
| `/subscribe` | Start receiving alerts (default: Jakarta) |
| `/unsubscribe` | Stop all alerts |
| `/check` | Current weather + upcoming rain forecast |
| `/status` | Your subscription info |
| `/help` | Show commands |
| 📍 *Send location* | Update your alert area to exact GPS position |

## Architecture

```
┌──────────────┐     ┌──────────────────┐     ┌──────────────┐
│   Telegram   │◄───►│  rain-alert-bot   │────►│  Open-Meteo  │
│   (users)    │     │  (Go, long poll)  │     │  (free API)  │
└──────────────┘     └──────────────────┘     └──────────────┘
                            │
                     ┌──────┴───────┐
                     │ Cron (3 hrs) │
                     │ Health :8080 │
                     │ subscribers  │
                     │   .json      │
                     └──────────────┘
```

## Project Structure

```
rain-alert-bot/
├── main.go           # All-in-one: bot, weather, alerts, cron, http
├── go.mod            # Go module
├── Dockerfile        # Multi-stage build (tiny Alpine image)
├── render.yaml       # Render.com deployment config
├── fly.toml          # Fly.io deployment config
├── .env.example      # Environment variable template
├── .gitignore
└── README.md
```

## Customization

### Change alert schedule
In `main.go`, edit the cron expression:
```go
c.AddFunc("0 5,8,11,14,17,20,23 * * *", func() { ... })
//        │ └─ hours (WIB)
//        └─ minute 0
```

### Change default location
Set `DEFAULT_LAT` and `DEFAULT_LON` environment variables, or edit the defaults in `LoadConfig()`.

### Add more weather conditions
Edit `classifyWeather()` in `main.go`. Full WMO code table is in the comments.

## Tech Stack

- **Go** — fast, single binary, great for bots
- **Open-Meteo** — free weather API, no key needed, CC BY 4.0
- **telegram-bot-api** — mature Go library for Telegram
- **robfig/cron** — in-process cron scheduler
- **Render / Fly.io** — free hosting with persistent storage

## License

MIT — do whatever you want with it.
