# define — instant word definitions (Wayland + GNOME notifications)

`define` is a tiny, fast CLI tool for Ubuntu (Wayland) that shows a GNOME notification with a definition for a word you type **or** a word you have **selected** (PRIMARY selection).  
It’s online-first with offline fallback, and it can run as a small user daemon for maximum speed.

---

## What it does

- `define <word>` → shows a notification with the definition
- Select text + run the shortcut command → shows a notification for the selected word (no copying needed)
- Click the notification → opens a full, scrollable view of the definition (Zenity)
- Online-first, then fallbacks:
  - ☁️ Online (dictionaryapi.dev)
  - 🧾 Online fallback (Wiktionary REST)
  - 🗄️ Offline fallback (local `dict` + GCIDE)
  - ❓ Not found

---

## Install (recommended)

- **From GitHub Releases (no Go needed):** download a prebuilt binary from **Releases**.
- **From source:** build with Go.

➡️ **Releases:** https://github.com/<rayan6ms/define/releases

## Install from GitHub Releases (no Go required)

1) Download the latest release

2) Extract and install to `~/.local/bin`:

```bash
# Example (adjust VERSION + arch):
VERSION="v0.1.0"
ARCH="linux-amd64"

cd ~/Downloads
tar -xzf "define-${VERSION}-${ARCH}.tar.gz"
install -Dm755 define "$HOME/.local/bin/define"
```

3) Make sure ~/.local/bin is on your PATH:

```bash
echo $PATH | grep -q "$HOME/.local/bin" || echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

4) Test:

```bash
define legends
```

## Requirements (Ubuntu)

### Required
- Go (for building)

### Recommended (for Wayland selection + full view + offline fallback)
- `wl-clipboard` (provides `wl-paste`) → selection support on Wayland
- `zenity` → opens a full scrollable window when you click the notification
- `dict` + GCIDE database → offline fallback

Install dependencies:

```bash
sudo apt update
sudo apt install -y golang wl-clipboard zenity dict dict-gcide
```

> If you skip `dict` / `dict-gcide`, offline fallback won’t work.
> If you skip `zenity`, clicking the notification won’t open a GUI full-view window (it’ll just do nothing useful).

---

## Build

From the repo root:

```bash
go mod tidy
go build -trimpath -ldflags="-s -w" -o define ./...
```

---

## Install (recommended location)

This project is designed to be installed into:

* `~/.local/bin/define`

Install it like this:

```bash
install -Dm755 ./define "$HOME/.local/bin/define"
```

Confirm:

```bash
ls -l "$HOME/.local/bin/define"
"$HOME/.local/bin/define" legends
```

---

## Usage

### Define a typed word

```bash
"$HOME/.local/bin/define" legends
```

### Define the currently selected word (Wayland PRIMARY selection)

Select a word with your mouse (highlight it), then run:

```bash
"$HOME/.local/bin/define"
```

This is the command you should use in your desktop shortcut.

### Open the last full definition

If a notification is truncated, you can open the full last definition with:

```bash
"$HOME/.local/bin/define" --full
```

---

## Keyboard shortcut (Wayland)

Set your shortcut command to:

```bash
$HOME/.local/bin/define
```

(Your desktop shortcut GUI may not expand `$HOME`. If it doesn’t, use the full absolute path for your user.)

---

## Optional: Run as a daemon (recommended)

Daemon mode makes lookups feel instant and improves click-to-open behavior because the process stays alive and can react to notification clicks.

### Create a user systemd service (runs on boot)

Create the file:

```bash
mkdir -p ~/.config/systemd/user
nano ~/.config/systemd/user/define.service
```

Paste:

```ini
[Unit]
Description=define dictionary daemon

[Service]
Type=simple
ExecStart=%h/.local/bin/define --daemon
Restart=on-failure

[Install]
WantedBy=default.target
```

Enable + start (this also makes it start automatically on boot/login):

```bash
systemctl --user daemon-reload
systemctl --user enable --now define.service
```

Check status:

```bash
systemctl --user status define.service
```

Restart if needed:

```bash
systemctl --user restart define.service
```

Stop + disable auto-start:

```bash
systemctl --user disable --now define.service
```

---

## Logs, cache, and resetting

`define` stores small local state in:

* **Cache directory:** `~/.cache/define/`
* **Cache file:** `~/.cache/define/cache.json`
  Stores cached definitions (speeds up repeat lookups).
* **Last definition:** `~/.cache/define/last.txt`
  Used by `--full` to open the last definition again.

You may want to reset if:

* you changed parsing/formatting and want fresh output
* you suspect you cached an undesirable result
* you want to shrink local stored data

### Reset everything

```bash
rm -rf ~/.cache/define
```

If you use the daemon, restart it after resetting:

```bash
systemctl --user restart define.service
```

> Note: debug logs only exist if you built/ran a version that writes debug logs. If you don’t see `debug.log`, that’s normal.

---

## Troubleshooting

### Online API doesn’t have a definition for a word

Example: `lemmatization` often returns “No Definitions Found” from dictionaryapi.dev.
In that case `define` will try Wiktionary, then fall back to offline `dict` (GCIDE) if installed.

### Selection doesn’t work on Wayland

Make sure `wl-clipboard` is installed and `wl-paste` works:

```bash
which wl-paste
wl-paste -p --no-newline | head -c 80; echo
```

---

## License

GPLv3 (see [LICENSE](LICENSE))
