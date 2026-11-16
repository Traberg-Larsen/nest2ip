# nest2ip

nest2ip gør Google Nest-kameraer til almindelige RTSP-streams på dit eget netværk.
Alt kører lokalt i Home Assistant. Intet sendes til andre end Google.

## Sådan virker det

- nest2ip holder **én** WebRTC-session med Google pr. kamera og deler den med alle RTSP-klienter.
  Flere TV'er, dashboards eller optagere giver ikke flere kald til Google.
- Sessionen forlænges ca. 60 sekunder før den udløber (Google giver 5 minutter ad gangen).
  Kan den ikke forlænges, åbnes en ny session, før den gamle lukker, så billedet ikke fryser.
- Alle kald til Google går gennem én kvote-vagt:
  højst 8 kald i minuttet for projektet, og højst 4 i minuttet og 60 i timen pr. kamera.
- Ved fejl venter nest2ip 1, 2, 5, 10 og derefter 30 minutter (plus lidt tilfældig spredning).
  Ved 429 fra Google respekteres `Retry-After`. Der er aldrig faste genforsøg hvert par sekunder.
- Kommer der ingen video inden for 20 sekunder efter forbindelsen, tæller det som en fejl.

## Opsætning

- **Google-login:** Med `use_home_assistant_login: true` genbruger nest2ip den Nest-login, som
  Home Assistants Nest-integration allerede har. Hemmeligheder vises aldrig i log eller status.
  Du kan også udfylde `client_id`, `client_secret` og `refresh_token` selv.
- **Kameraer:** `name` skal svare til kameraets navn hos Google (store/små bogstaver og æøå er ligegyldige).
  `port` og `path` bestemmer RTSP-adressen: `rtsp://<HA-IP>:<port>/<path>`.
  `muted_path` er valgfri og giver en ekstra adresse med kun video.
  `always_on: false` åbner kun strømmen, mens nogen ser med, og holder den åben i `idle_timeout_minutes` bagefter.

Eksempel:

```yaml
cameras:
  - name: Have
    port: 39425
    path: en-lang-tilfældig-streng
    muted_path: en-anden-lang-streng
```

## Status

- Panelet **nest2ip** i Home Assistants sidepanel viser hvert kamera: tilstand, seneste billede, kald i minuttet og timen, og hvornår næste forsøg er tilladt.
- `http://<HA-IP>:8787/health` giver det samme som JSON (ingen hemmeligheder).
- `http://<HA-IP>:8787/snapshot/<kamera>.jpg?key=<path>` giver et stillbillede fra den kørende strøm, uden kald til Google.
