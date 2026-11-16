# nest2ip

Google Nest-kameraer som almindelige RTSP-streams, kørt lokalt som en Home Assistant-app.

- Én WebRTC-session med Google pr. kamera, delt af alle RTSP-klienter.
- Kvote-vagt foran alle kald til Google (Device Access sandbox-grænser), med backoff ved fejl og pause ved 429.
- Sessioner forlænges før de udløber; ellers skiftes til en ny session, uden at klienterne mister forbindelsen.
- Status i Home Assistants sidepanel og som JSON på `/health`. Stillbilleder fra den kørende strøm på `/snapshot/<kamera>.jpg`.

## Installation i Home Assistant

1. **Indstillinger → Apps → App-butik → ⋮ → Repositories**, og tilføj `https://github.com/Traberg-Larsen/nest2ip`.
2. Installér **nest2ip**, udfyld kameraerne under **Konfiguration**, og start appen.

Se [nest2ip/DOCS.md](nest2ip/DOCS.md) for opsætning.

## Udvikling

```bash
cd nest2ip
go test ./...
go test -tags integration ./...   # kræver ffmpeg
```
