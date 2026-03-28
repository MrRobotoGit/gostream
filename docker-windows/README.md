# Docker-Windows (planned): GoStream + Plex/Jellyfin single-container stack

This folder documents the **Windows-only** installer plan for generating an **auto-contained Dockge stack** that runs **one container**:

- `gostream-plex` **or** `gostream-jellyfin` (you choose during install)

The installer itself (`install-rebuild.bat`) and templates will live under `docker-windows/` in later tasks. **This task only adds documentation.**

---

## Prerequisites (non-negotiable)

### Docker Desktop must run **Linux containers (WSL2 engine)**

GoStream runs Linux containers; Windows containers won’t work.

Preflight:

```powershell
docker version
docker info --format "{{.OSType}}"  # must print: linux
```

If `OSType` is `windows`, switch Docker Desktop to **Linux containers** and ensure WSL2 is enabled.

### FUSE must be available as `/dev/fuse`

GoStream mounts a FUSE filesystem inside the container. Docker Desktop setups vary; **FUSE may fail even if everything else works**.

Preflight probe (expected to list `/dev/fuse`):

```powershell
docker run --rm --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined alpine:3.20 ls -l /dev/fuse
```

Common failures:

- `ls: /dev/fuse: No such file or directory` → FUSE device not exposed to Linux containers in this Docker Desktop/WSL2 setup
- `permission denied` → insufficient container permissions / security policy

---

## What this does

When implemented, `docker-windows\install-rebuild.bat` will:

1. Prompt for **flavor**: Plex vs Jellyfin
2. Prompt for **paths** (no hardcoded `B:\...`):
   - Base data directory (default suggestion: `%USERPROFILE%\Documents\Docker Stuff`)
   - Dockge stacks root (default suggestion: `%USERPROFILE%\Documents\Docker Stuff\Dockge\stacks`)
3. Create/repair the expected base folder structure **idempotently** (never delete)
4. Generate an **auto-contained** Dockge stack folder at:
   - `{stacksRoot}\gostream-plex\` or `{stacksRoot}\gostream-jellyfin\`
5. Optionally (Mode A) recreate the target container **without wiping media/config**

`install-rebuild.bat` now always pauses before closing, so you can read success/errors even when launched by double click.

By default it generates stack files from built-in templates under `docker-windows/templates`.

If you already created your own deploy files, you can point the installer to them:

- existing `compose.yaml`
- existing `Dockerfile`

### Paths and config rule (repo-specific)

The GoStream config file is expected at:

`{base}\gostream-mkv-real\config\config.json`

The installer must create it from `config.json.example` if missing.

### Rebuild semantics (Mode A)

**Mode A = recreate container only**:

- Stop/remove the existing container with the same name (only)
- Recreate it
- **Never wipe** `{base}\gostream-mkv-real\{movies|tv|config}` or other data directories

---

## What this does NOT do

- Does **not** install Plex/Jellyfin on Windows (everything is containerized)
- Does **not** use `docker system prune`, volume prune, or `down -v`
- Does **not** guarantee FUSE will work on every Docker Desktop setup

---

## Interactive flow (planned)

The installer will ask (in this order):

1. **my-deploy quick choice** (if files exist):
   - Use `docker-windows/my-deploy/my-deploy.compose.yaml` + `my-deploy.Dockerfile` (default **No**)
   - If you choose **Yes**, installer skips remaining interactive questions, materializes compose into `B:\Documents\Docker Stuff\Dockge\stacks\gostream-plex\compose.yaml`, auto-adjusts busy host ports, and deploys from that stack folder
2. **Flavor**: Plex or Jellyfin
3. **Base path** (data root)
4. **Stacks root** (where Dockge reads stacks)
5. **Install source**:
   - Start from scratch (built-in templates), or
   - Use existing files you already created (compose + Dockerfile)
   - (Skipped automatically if my-deploy was selected in step 1)
6. **Runtime defaults**:
   - Choose recommended defaults (`PUID=1000`, `PGID=1000`, `TZ=America/La_Paz`) or enter custom values
7. **Plex import** (Plex flavor only; see below)

Container names:

- Plex: `gostream-plex`
- Jellyfin: `gostream-jellyfin`

---

## Non-interactive flags (planned)

Used for deterministic verification and CI-like runs:

```text
install-rebuild.bat --flavor plex|jellyfin --mode A --base "..." --stacks "..." --non-interactive

install-rebuild.bat --flavor plex|jellyfin --mode A --base "..." --stacks "..." --non-interactive --use-existing-files --compose-file "..." --dockerfile-file "..."
```

Optional deploy opt-out:

```text
--no-deploy
```

Notes:

- `--base` and `--stacks` must be explicit in non-interactive mode.
- Paths are user-provided; README examples may use `%USERPROFILE%` placeholders.
- If `--use-existing-files` is set, both `--compose-file` and `--dockerfile-file` are required.

---

## Port auto-selection behavior

For template-based stacks, host port conflict avoidance is based on ports already bound by **existing Docker containers** (via `docker ps` / `docker inspect`).

In **my-deploy mode**, auto-port selection checks both:

- active TCP listeners on Windows host
- existing Docker published ports

In **my-deploy mode**, before `docker compose up`, the installer:

1. writes a stack compose at `B:\Documents\Docker Stuff\Dockge\stacks\gostream-plex\compose.yaml`
2. generates a temporary `*.autoports.yaml` next to it if any published host port is already in use
3. deploys with that auto-adjusted compose file

Planned host port bases (first choice; if taken, increment within a safe range):

- Plex: `32400`
- Jellyfin: `8096` (+ optional `8920`)
- GoStorm API: `8090`
- Health monitor: `8095`
- Metrics/control/webhook: derived from GoStream `metrics_port` (prefer same host port)

---

## Plex import feature (Windows Plex → Linux container)

If you already run Plex on Windows, the installer (Plex flavor) will offer an **optional import**.

Defaults/suggestions:

- Source (Windows): `%LOCALAPPDATA%\Plex Media Server`
- Destination (inside container-mounted `/config`, Linux-style path on disk):
  - `{base}\gostream-plex\config\Library\Application Support\Plex Media Server\`

Critical rules:

- **Stop Plex on Windows first**. If files are locked, the import must abort with a clear message.
- **Windows-only Plex plugins/scanners may not work on Linux**. Expect that some plug-ins, scanners, or binary components won’t load.

---

## Troubleshooting notes (planned)

- If FUSE preflight fails, the stack cannot run GoStream inside Docker Desktop reliably in this environment.
- If Plex import completes but Plex behaves oddly, re-test with a clean container config (no imported plugins) to isolate Windows-to-Linux incompatibilities.
