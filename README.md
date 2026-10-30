# chatbox

## Konfiguration

| Variable     | Default | Beschreibung                                                  |
|--------------|---------|---------------------------------------------------------------|
| `PORT`       | `3000`  | HTTP-Port                                                     |
| `API_KEY`    | `dummy` | Bearer-Key, den Open WebUI schickt (`Authorization: Bearer <key>`). In Produktion unbedingt setzen. |
| `LOG_LEVEL`  | `info`  | `debug`, `info`, `warn`, `error`. `debug` loggt alle Request-Header (`Authorization`/`Cookie` geschwärzt). |
| `LOG_FORMAT` | `text`  | `text` oder `json`                                            |
| `TERMINAL_BACKEND` | `kubernetes` | Wo die Terminals laufen: `kubernetes` oder `docker` |
| `TERMINAL_IMAGE` | `ghcr.io/open-webui/open-terminal:latest` | Image, wenn die Policy keins vorgibt |
| `TERMINAL_IDLE_TIMEOUT_MINUTES` | `30` | Pods ohne Request und ohne offenes Terminal werden danach gelöscht, wenn die Policy nichts vorgibt. `0` = nie |
| `TERMINAL_PROVISION_TIMEOUT_SECONDS` | `120` | Maximale Wartezeit, bis ein neuer Pod bereit ist. Muss unter den 300 s bleiben, die Open WebUI wartet |
| `TERMINAL_NAMESPACE` | `chatbox-terminals` | Kubernetes: Namespace für die Open-Terminal-Pods |
| `TERMINAL_STORAGE_CLASS` | (Cluster-Default) | Kubernetes: StorageClass für PVCs von Policies mit `storage` |
| `KUBECONFIG` | `~/.kube/config` | Kubernetes: nur außerhalb des Clusters; im Cluster wird der Service-Account genutzt |
| `TERMINAL_DOCKER_NETWORK` | – | Docker: Netz, in dem chatbox und die Terminal-Container hängen (Pflicht) |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker: Engine-API, `unix://` oder `tcp://` ohne TLS |

## Wie Terminals bereitgestellt werden

Pro (User, Policy, Context) startet chatbox eine eigene Open-Terminal-Instanz. Ihr Name ist ein Hash aus
diesen drei Werten, chatbox findet seine Instanzen nach einem Neustart also wieder. Der erste Request eines
Users wartet, bis die Instanz bereit ist.

Requests leitet chatbox direkt an die IP der Instanz weiter: `Authorization` wird gegen deren Key
getauscht, `X-User-Id` und `X-Terminal-Context-Id` werden entfernt, `X-Session-Id` bleibt. Das interaktive
Terminal (WebSocket) wird von chatbox angenommen, per erster Nachricht authentifiziert und durchgereicht.
Instanzen ohne Request und ohne offenes Terminal werden nach dem Idle-Timeout gelöscht.

Policies und Aktivität liegen im Speicher: genau eine Replica, Policies gehen bei einem Neustart verloren.

**Kubernetes** (`TERMINAL_BACKEND=kubernetes`, Default): Pod im Namespace `TERMINAL_NAMESPACE`, Secret mit
dem API-Key, bei gesetztem `storage` ein PVC unter `/home/user`. chatbox muss **im Cluster laufen**, siehe
`deploy/kubernetes.yaml`.

**Docker** (`TERMINAL_BACKEND=docker`): Geschwister-Container über den Docker-Socket im Netz
`TERMINAL_DOCKER_NETWORK`, bei gesetztem `storage` ein Docker-Volume unter `/home/user` (die Größe setzt
Docker nicht durch). CPU- und Speicherlimits gelten. `pod_security_context`, `container_security_context`
und `restricted` sind Kubernetes-spezifisch und werden ignoriert. Zugriff auf den Docker-Socket ist
praktisch Root auf dem Host, deshalb nur für Entwicklung oder einzelne vertrauenswürdige Hosts.

Noch nicht umgesetzt: geplante Resets (`reset.schedule` im Lifecycle), `storage_mode: shared`,
`pod_template` und der JWT-Modus (`auth_type: session`).

## Lokal testen

```sh
LOG_LEVEL=debug go run ./cmd/chatbox
curl -i localhost:3000/health          # {"status":"ok"}
curl -i -H "Authorization: Bearer dummy" localhost:3000/api/v1/policies
curl -i -X PUT -H "Authorization: Bearer dummy" -d '{"cpu_limit":"1"}' localhost:3000/api/v1/policies/dev
curl -i -H "Authorization: Bearer dummy" -H "X-User-Id: u1" localhost:3000/p/dev/files/list
```

## API

Die Schnittstelle ist in `openapi.json` beschrieben (Open Terminal 0.14.6 und der Orchestrator
Terminals 0.2.5). Daraus werden Server-Interface und Modelle generiert:

```sh
go generate ./internal/api
```

Generiert werden nur die Operationen, die der Orchestrator selbst behandelt (siehe
`include-tags` in `internal/api/oapi-codegen.yaml`). Alle übrigen Open-Terminal-Pfade laufen über den
Catch-all-Proxy zum Pod.

`chatbox healthcheck` prüft `/health` des laufenden Servers und wird im Docker-Image als `HEALTHCHECK` genutzt.

## Mit Open WebUI (Docker Compose)

```sh
cp .env.example .env            # CHATBOX_API_KEY setzen
docker compose up               # baut chatbox immer neu; Open WebUI: http://localhost:8084
```

Ollama muss auf dem Host auf Port `11434` laufen. In Open WebUI unter Admin → Einstellungen den Terminal-Server
`http://chatbox:3000` mit Auth „Bearer“ und dem Wert von `CHATBOX_API_KEY` eintragen.

Compose nutzt das Docker-Backend: chatbox startet die Terminals als eigene Container im Netz `chatbox`.
Sie gehören nicht zum Compose-Projekt. Vor `docker compose down` deshalb zuerst entfernen, sonst kann
Compose das Netz nicht löschen:

```sh
docker rm -f $(docker ps -aq --filter label=chatbox.managed=true)
docker volume rm $(docker volume ls -q --filter label=chatbox.managed=true)   # optional: Daten löschen
```
