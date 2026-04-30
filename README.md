# he2en-tts

Browser-based text-to-speech using Google Cloud TTS, ElevenLabs, or Cartesia. No install required.

## Try it now

→ **[guybernstein.github.io/he2en-tts](https://guybernstein.github.io/he2en-tts/)**

Open the link, paste an API key from your TTS provider of choice, type some text, click **Generate speech**. Your key is saved to your browser's `localStorage` and sent only to the provider's official API — never to any third-party server.

### Supported providers

| Provider | Voice config | Where to get a key |
|---|---|---|
| **Google Cloud TTS** | 30+ Chirp3 HD voices + 10 Standard voices, any `languageCode` | [Cloud Console](https://console.cloud.google.com/apis/credentials) |
| **ElevenLabs** | Voice ID + model (v3, multilingual_v2, turbo_v2_5, flash_v2_5) | [API keys](https://elevenlabs.io/app/settings/api-keys) |
| **Cartesia** | Voice ID + model (sonic-3, sonic-2) | [Cartesia keys](https://play.cartesia.ai/keys) |

The web UI calls each provider's REST API directly from the browser using `fetch()`. All three support CORS for browser clients with the right auth header. Output is requested as MP3 from all three for uniform `<audio>` playback.

## CLI (optional)

If you prefer the terminal, this repo is also a small Go program with provider fallback, monthly budget tracking, and morning/evening voice profiles.

```bash
git clone https://github.com/GuyBernstein/he2en-tts
cd he2en-tts
go build -o he2en-tts .
cp tts_config.example.json tts_config.json
# edit tts_config.json — enable providers, set voice IDs
export GEMINI_API_KEY=...           # or ELEVENLABS_API_KEY / CARTESIA_API_KEY
./he2en-tts --config tts_config.json --text "Hello, world."
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--config FILE` | Path to JSON config (see `tts_config.example.json`) |
| `--text "..."` | Text to synthesize |
| `--output FILE.wav` | Output path (default `tts_output.wav`) |
| `--period morning\|evening` | Force a profile instead of auto-detecting by local time |
| `--test` | Probe every enabled provider for both profiles, print results |
| `--budget` | Print month-to-date usage per tier |
| `--verbose` | Log provider attempts and fallback reasons to stderr |

Budget state is kept in `~/.he2en-tts/tts_usage.json` and resets monthly.

## Repo layout

```
.
├── docs/
│   └── index.html           # the web UI (served by GitHub Pages)
├── main.go                  # Go CLI (~600 lines, no external deps)
├── go.mod
└── tts_config.example.json  # copy → tts_config.json, edit
```

## Adding more providers or voices

Each provider is implemented as a single `providerFunc` in `main.go`. To add one, write a function with the signature `func(ProviderConfig, VoiceProfile, string) ([]byte, error)`, register it in the `providers` map, and add a matching entry to `tts_config.json`. The web UI mirrors the same shape — add a tab and a `tts<Provider>(text)` async function in `docs/index.html`.
