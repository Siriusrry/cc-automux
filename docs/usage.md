# Usage

[简体中文](usage.zh-CN.md) · [README](../README.md)

## Install or run directly

Build with Go 1.22 or newer:

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
```

For a per-user service on macOS or Linux:

```bash
./scripts/install.sh
```

The installer prompts for a management key on first use and preserves an existing configuration on reinstall. macOS uses a LaunchAgent; Linux uses `systemd --user`. Installation enables autostart. See the [script reference](../scripts/README.md) for platform paths and removal behavior.

To run directly instead:

```bash
./dist/cc-automux init --generate-management-key
./dist/cc-automux
```

Do not start a second instance on the installed service's port. To choose a separate configuration, use an absolute path:

```bash
./dist/cc-automux init --generate-management-key --config /absolute/path/config.json --listen-addr 127.0.0.1:8766
CC_AUTOMUX_CONFIG=/absolute/path/config.json ./dist/cc-automux
```

`init` without `--generate-management-key` prompts for a key. Repeating initialization preserves an existing configuration and key. `./dist/cc-automux --version` prints the embedded product version.

## Sign in and connect Claude Code

Open [http://127.0.0.1:8765/management](http://127.0.0.1:8765/management) with the configured port and sign in with the management key. By default the browser stores it for the tab session; **Remember on this device** stores it persistently in that browser. **Sign out** clears the stored key.

1. In **Service**, generate or enter a gateway key. It must differ from the management key. Without it, Messages requests return `503 gateway_not_configured`.
2. In **Providers**, add a compatible upstream, its key, and exact model names.
3. In **Claude Code**, create a named profile with the four required mappings: Haiku, Sonnet, Opus, and Fable. Each mapping names a model declared by a provider. Subagent and Teammate mappings are optional.
4. Activate the profile. CC AutoMux writes the gateway address/key and mappings into the selected Claude Code settings file.
5. Start a new Claude Code session so it reads the configuration.

For manual setup, use the gateway's base address without an endpoint suffix:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

Set Claude Code's model mappings separately if you do not activate a profile.

## Console pages

The overview uses `/management`; other pages use `/management#/providers`, `/management#/logs`, and similar hash paths. There is no slash before `#`.

| Page | Purpose |
|---|---|
| Overview | Runtime status, provider counts, routing map, and configuration warnings. |
| Providers | Add/edit/remove upstreams, declare models, set priorities and patches, and inspect health and sessions. |
| Auto Mode | Select Off, Provider pool, or Fixed provider for classifier requests. |
| Claude Code | Select the settings path, configure telemetry, and create/edit/activate model-mapping profiles. |
| Logs | Search retained history, follow live records, expand errors, and load older records. |
| Service | Set the listener, log limit and keys; view runtime details and configuration JSON. |

Forms with a save bar require **Save changes**; **Revert** discards the draft. Inline switches and Profile actions apply immediately as indicated. The settings path uses **Apply path**. Failed saves retain the input and show an error. If another window changes the same configuration field, reload the current values before retrying; unrelated concurrent changes are preserved.

### Providers and routing

Ordinary upstreams must accept the Anthropic Messages API. Enter their base URL; CC AutoMux appends `/v1/messages`. Do not append that endpoint yourself. Model names are case-sensitive and matched exactly.

Higher numeric priorities are tried first. New sessions round-robin within the highest available priority; sessions with a valid `X-Claude-Code-Session-Id` stay on a provider for the same model and request type. Lower tiers are used when higher ones are unavailable. Normal requests may try up to three different providers on eligible failures. Disabling health cooldown prevents failure-based scheduling suppression; diagnostics remain available.

TLS uses system roots by default. A custom CA file and skipping certificate verification are mutually exclusive. Compatibility patches are selected explicitly and run in the chosen order; provider names and URLs do not enable them automatically.

### Auto mode

- **Off:** classifier requests return `503 auto_mode_not_configured`; normal requests still use the provider pool.
- **Provider pool:** set the shared classifier model. Enabled providers declaring that model are candidates, using priority, stickiness, and a separate classifier health channel. A classifier request makes at most one provider attempt.
- **Fixed provider:** supply a base URL, key, protocol, TLS settings, and optional classifier patches. This target serves only classifier requests and does not join the provider pool.

Anthropic Messages fixed targets work directly. OpenAI Responses and OpenAI-compatible conversion are not implemented; saving those selections is allowed, but classifier requests return `501 protocol_not_implemented`. Failed parsing or conversion never produces a fabricated allow/block result.

### Claude Code files and profiles

The default target is the current user's `.claude/settings.json`; you can select an absolute custom path. Activation updates only the managed gateway, model, and telemetry fields and preserves other values. Before the first modification of an existing file, CC AutoMux creates a sibling `.cc-automux.bak`; an existing backup is never overwritten. Creating a new settings file does not create a backup.

Saving a profile does not activate it. **Active** means the managed file contents were verified; changing them outside CC AutoMux clears that state on the next check. Open or return to the Claude Code page to refresh it. Changing the gateway address/key or the active profile can require reactivation.

**Disable Claude Code telemetry** controls the four fields shown beside the switch. Its value is written when a profile is activated.

### Logs and restarts

The process writes structured JSON Lines to one active log and one archive. Use **Logs** for retained history and live events. Filters apply to both. Scrolling up pauses following while new records buffer; return to the bottom to resume. A dropped-record or full-buffer notice asks you to reload the view.

Long fields are initially bounded. **Show complete** loads the retained full record; if rotation has removed it, the page says so. A logging-health warning means writes are failing even if the gateway is still serving.

Changing the listener port or log size limit restarts the process and interrupts in-flight requests. The console waits for the new configuration. A port change opens the new console address and requires a new sign-in. Rotating only the management key keeps this console signed in with the new key; other sessions must sign in again.

## Configuration and management API

Default configuration paths:

| Platform | Path |
|---|---|
| macOS | `~/Library/Application Support/cc-automux/config.json` |
| Linux | `$XDG_CONFIG_HOME/cc-automux/config.json`, or `~/.config/cc-automux/config.json` |

`CC_AUTOMUX_CONFIG` overrides the file using an absolute path. Running instances apply changes through the console or management API; editing the disk file requires a process restart.

A minimal configuration has this shape; replace the example management key before use:

```json
{
  "schema_version": 1,
  "service": {"listen_addr": "127.0.0.1:8765", "log_max_bytes": 104857600},
  "auth": {"gateway_key": "", "management_key": "replace-with-a-random-management-key"},
  "auto_mode": {"mode": "disabled", "model": ""},
  "harnesses": {"claude_code": {"path_mode": "default", "settings_path": "", "disable_telemetry": true, "profiles": []}},
  "providers": []
}
```

The service requires a management key and only binds `127.0.0.1`. Unix configuration files use mode `0600`. Keep configuration files and keys out of source control.

Authenticated management requests use `Authorization: Bearer <management key>`:

- `GET /api/v1/status`: runtime, restart, and logging health.
- `GET/PUT /api/v1/config`: complete configuration read/replacement.
- `/api/v1/providers` and `/api/v1/providers/{id}`: provider CRUD.
- `/api/v1/harnesses/claude-code` and its `/profiles` resources: settings and profiles.
- `GET /api/v1/logs`, `/api/v1/logs/stream`, `/api/v1/logs/record?ref=…`: history, SSE, and complete records.

Config GET includes server-owned `active_profile_id`; remove it when constructing a config PUT. Preserve fields you are not changing. Send the GET response's ETag in `If-Match` to reject a stale replacement with `412 configuration_changed`. A successful PUT returns `200` for applied settings or `202` for a pending restart.

## Troubleshooting

- **Console unreachable:** check `./scripts/status.sh`, the configured port, and startup errors. macOS writes early errors to `~/Library/Logs/cc-automux/bootstrap.log`; Linux uses `journalctl --user -u cc-automux.service`.
- **Sign-in rejected / 401:** use the management key for the console and the gateway key for Claude Code. A rotated management key invalidates other sessions.
- **Model unavailable:** check the exact requested model, enabled providers, and their health. Open Providers for the upstream's original error and diagnostics.
- **Auto mode fails:** set its classifier model and a usable candidate or Anthropic fixed target. Unsupported OpenAI protocols return 501.
- **Save conflict / 412:** keep a copy of the draft if needed, then load current values and reapply the intended change.
- **Profile is out of sync:** inspect the selected path and reactivate the intended profile. On write failure the page reports an error instead of declaring it active.
- **Reinstall did not reset settings:** existing configuration and keys are deliberately preserved.

For service start/stop, upgrades, log paths, and uninstall behavior, see the [script reference](../scripts/README.md).
