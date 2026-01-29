// define — instant word definitions (Wayland + GNOME notifications)
// Copyright (C) 2026 Rayan rayan6ms@gmail.com
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	socketName    = "define.sock"
	appName       = "define"
	apiTimeout    = 900 * time.Millisecond
	cmdTimeout    = 180 * time.Millisecond
	daemonReadMax = 4096

	maxWordLen   = 64
	memCacheMax  = 2500
	cacheTTL     = 30 * 24 * time.Hour
	dedupeWindow = 250 * time.Millisecond

	bodyMaxChars = 1400

	primaryAPI    = "https://api.dictionaryapi.dev/api/v2/entries/en/%s"
	wiktionaryAPI = "https://en.wiktionary.org/api/rest_v1/page/definition/%s"

	offlineRefreshAfter = 12 * time.Hour
)

var (
	wordRe         = regexp.MustCompile(`^[\w\-']+$`)
	wsCollapseRe   = regexp.MustCompile(`\s+`)
	bracketTagRe   = regexp.MustCompile(`\s*\[[^\]]+\]`)          // removes [PJC], [1913 Webster], etc.
	dbHeaderLineRe = regexp.MustCompile(`^[A-Za-z0-9_-]+:\s+.+$`) // "gcide: Legend"
)

type config struct {
	debug       bool
	daemon      bool
	forceOnline bool
	noOffline   bool
	fullView    bool
}

type paths struct {
	wlPaste string
	dict    string
	zenity  string
}

func main() {
	cfg := parseArgs(os.Args[1:])
	ensureCommonPATH()
	p := resolvePaths()

	if cfg.fullView {
		openFullFromLast(p)
		return
	}

	if cfg.daemon {
		os.Exit(runDaemon(cfg, p))
	}

	word := ""
	args := filterOutFlags(os.Args[1:])
	if len(args) > 0 {
		word = pickWord(strings.Join(args, " "))
	} else {
		word = pickWord(getSelectedTextWayland(cfg, p))
	}
	if !validWord(word) {
		return
	}

	_ = clientSend(cfg, word)
}

func parseArgs(args []string) config {
	cfg := config{}
	for _, a := range args {
		switch a {
		case "--debug":
			cfg.debug = true
		case "--daemon":
			cfg.daemon = true
		case "--force-online":
			cfg.forceOnline = true
		case "--no-offline":
			cfg.noOffline = true
		case "--full":
			cfg.fullView = true
		}
	}
	return cfg
}

func filterOutFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func ensureCommonPATH() {
	p := os.Getenv("PATH")
	need := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	for _, d := range need {
		if !strings.Contains(p, d) {
			p = d + ":" + p
		}
	}
	_ = os.Setenv("PATH", p)
}

func resolvePaths() paths {
	look := func(bin string) string {
		p, _ := exec.LookPath(bin)
		return p
	}
	return paths{
		wlPaste: look("wl-paste"),
		dict:    look("dict"),
		zenity:  look("zenity"),
	}
}

func runtimeSocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, socketName)
}

func cacheDir() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	dir = filepath.Join(dir, "define")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func cacheFilePath() string { return filepath.Join(cacheDir(), "cache.json") }
func lastFilePath() string  { return filepath.Join(cacheDir(), "last.txt") }

func runCmdCapture(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var outb bytes.Buffer
	cmd.Stdout = &outb
	err := cmd.Run()
	return strings.TrimSpace(outb.String()), err
}

func getSelectedTextWayland(cfg config, p paths) string {
	if p.wlPaste == "" {
		return ""
	}
	if out, _ := runCmdCapture(p.wlPaste, "-p", "--no-newline"); out != "" {
		return out
	}
	if out, _ := runCmdCapture(p.wlPaste, "--no-newline"); out != "" {
		return out
	}
	return ""
}

func pickWord(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, " \t\r\n\"“”‘’.,;:!?()[]{}")
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func validWord(w string) bool {
	return w != "" && len(w) <= maxWordLen && wordRe.MatchString(w)
}

func lemmaCandidates(w string) []string {
	w = strings.ToLower(w)
	cands := []string{w}
	if strings.HasSuffix(w, "ies") && len(w) > 4 {
		cands = append(cands, w[:len(w)-3]+"y")
	}
	if strings.HasSuffix(w, "es") && len(w) > 4 {
		cands = append(cands, w[:len(w)-2])
	}
	if strings.HasSuffix(w, "s") && len(w) > 3 && !strings.HasSuffix(w, "ss") {
		cands = append(cands, strings.TrimSuffix(w, "s"))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func cap1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type diskEntry struct {
	Title  string    `json:"title"`
	Body   string    `json:"body"` // clamped
	Full   string    `json:"full"` // full text
	TS     time.Time `json:"ts"`
	Source string    `json:"source"` // online|wiktionary|offline|none
}

func loadDiskCache(path string) map[string]diskEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]diskEntry{}
	}
	var m map[string]diskEntry
	if json.Unmarshal(b, &m) != nil {
		return map[string]diskEntry{}
	}
	return m
}

func saveDiskCacheAtomic(path string, m map[string]diskEntry) {
	tmp := path + ".tmp"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func writeLast(full string) {
	_ = os.WriteFile(lastFilePath(), []byte(full), 0o600)
}

type cacheItem struct {
	key   string
	title string
	body  string
	full  string
	ts    time.Time
	src   string
}

type lruCache struct {
	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
	max   int
	ttl   time.Duration
}

func newLRU(max int, ttl time.Duration) *lruCache {
	return &lruCache{ll: list.New(), items: make(map[string]*list.Element, max), max: max, ttl: ttl}
}

func (c *lruCache) get(key string) (cacheItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*cacheItem)
		if time.Since(it.ts) > c.ttl {
			c.ll.Remove(el)
			delete(c.items, key)
			return cacheItem{}, false
		}
		c.ll.MoveToFront(el)
		return *it, true
	}
	return cacheItem{}, false
}

func (c *lruCache) set(key, title, body, full, src string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*cacheItem)
		it.title, it.body, it.full, it.ts, it.src = title, body, full, time.Now(), src
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheItem{key: key, title: title, body: body, full: full, ts: time.Now(), src: src})
	c.items[key] = el
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		if last == nil {
			break
		}
		old := last.Value.(*cacheItem)
		c.ll.Remove(last)
		delete(c.items, old.key)
	}
}

type dictAPIEntry struct {
	Word     string `json:"word"`
	Meanings []struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions  []struct {
			Definition string `json:"definition"`
			Example    string `json:"example"`
		} `json:"definitions"`
	} `json:"meanings"`
}

func lookupPrimary(client *http.Client, word string) (string, error) {
	url := fmt.Sprintf(primaryAPI, word)
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "define/1.0 (go)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("non-2xx")
	}

	var entries []dictAPIEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return "", err
	}
	if len(entries) == 0 || len(entries[0].Meanings) == 0 {
		return "", errors.New("no meanings")
	}

	var b strings.Builder
	added := 0
	for _, m := range entries[0].Meanings {
		if len(m.Definitions) == 0 {
			continue
		}
		if added > 0 {
			b.WriteString("\n\n")
		}
		if m.PartOfSpeech != "" {
			b.WriteString(m.PartOfSpeech)
			b.WriteString("\n")
		}
		d := m.Definitions[0]
		if d.Definition != "" {
			b.WriteString(d.Definition)
		}
		if d.Example != "" {
			b.WriteString("\nExample: ")
			b.WriteString(d.Example)
		}
		added++
		if added >= 3 {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("empty")
	}
	return out, nil
}

type wiktionaryDef struct {
	Definitions []string `json:"definitions"`
}

func lookupWiktionary(client *http.Client, word string) (string, error) {
	url := fmt.Sprintf(wiktionaryAPI, word)
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "define/1.0 (go)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("non-2xx")
	}

	var payload map[string][]wiktionaryDef
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	defs := payload["en"]
	if len(defs) == 0 {
		return "", errors.New("no en defs")
	}

	var b strings.Builder
	count := 0
	for _, bucket := range defs {
		for _, d := range bucket.Definitions {
			dd := strings.TrimSpace(d)
			if dd == "" {
				continue
			}
			if count > 0 {
				b.WriteString("\n")
			}
			dd = strings.ReplaceAll(dd, "[", "")
			dd = strings.ReplaceAll(dd, "]", "")
			b.WriteString("• ")
			b.WriteString(dd)
			count++
			if count >= 7 {
				break
			}
		}
		if count >= 7 {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("empty")
	}
	return out, nil
}

func normalizeOfflineLine(ln string) string {
	ln = strings.TrimRight(ln, "\r")
	ln = strings.TrimSpace(ln)
	if ln == "" {
		return ""
	}
	ln = wsCollapseRe.ReplaceAllString(ln, " ")
	ln = bracketTagRe.ReplaceAllString(ln, "")
	return strings.TrimSpace(ln)
}

func offlineLookup(p paths, word string) (string, error) {
	if p.dict == "" {
		return "", errors.New("dict not installed")
	}
	cmd := exec.Command(p.dict, "-d", "gcide", word)
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command(p.dict, "-m", word)
		out, err = cmd.Output()
		if err != nil {
			return "", err
		}
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", errors.New("empty")
	}
	if strings.Contains(raw, "No definitions found for") || strings.Contains(raw, "perhaps you mean") {
		return "", errors.New("no defs")
	}

	lines := strings.Split(raw, "\n")

	clean := make([]string, 0, 48)
	started := false
	prevBlank := false

	for _, ln := range lines {
		trim := strings.TrimSpace(ln)

		if strings.HasPrefix(ln, "From ") ||
			strings.HasPrefix(ln, "Database") ||
			strings.Contains(ln, "definition found") ||
			strings.HasPrefix(ln, "Copyright") ||
			strings.HasPrefix(ln, "dictd") ||
			strings.HasPrefix(ln, "----") {
			continue
		}
		if trim == "." {
			continue
		}
		if dbHeaderLineRe.MatchString(trim) && len(clean) == 0 {
			continue
		}

		if trim == "" {
			if started && !prevBlank && len(clean) > 0 {
				clean = append(clean, "")
				prevBlank = true
			}
			continue
		}

		norm := normalizeOfflineLine(ln)
		if norm == "" {
			continue
		}

		if !started {
			if strings.HasPrefix(norm, "1.") ||
				strings.HasPrefix(norm, "2.") ||
				strings.HasPrefix(norm, "The ") ||
				strings.HasPrefix(norm, "A ") ||
				strings.HasPrefix(norm, "An ") {
				started = true
			} else {
				continue
			}
		}

		clean = append(clean, norm)
		prevBlank = false
		if len(clean) >= 48 {
			break
		}
	}

	outStr := strings.TrimSpace(strings.Join(clean, "\n"))
	if outStr == "" {
		return "", errors.New("no usable offline content")
	}
	return outStr, nil
}

func clampBody(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= bodyMaxChars {
		return s
	}
	head := s[:bodyMaxChars-80]
	return strings.TrimSpace(head) + "\n\n… (click to open full)"
}

func sourceEmoji(src string) string {
	switch src {
	case "online":
		return "☁️"
	case "wiktionary":
		return "🧾"
	case "offline":
		return "🗄️"
	default:
		return "❓"
	}
}

func resolveDefinition(cfg config, p paths, mem *lruCache, disk map[string]diskEntry, diskDirty *bool, word string, client *http.Client) (title, body, full, source string) {
	key := strings.ToLower(word)

	if it, ok := mem.get(key); ok {
		return it.title, it.body, it.full, it.src
	}

	if de, ok := disk[key]; ok {
		if de.Source == "offline" && time.Since(de.TS) > offlineRefreshAfter {
		} else if time.Since(de.TS) <= cacheTTL {
			mem.set(key, de.Title, de.Body, de.Full, de.Source)
			return de.Title, de.Body, de.Full, de.Source
		}
	}

	var out, used string
	source = "none"

	for _, cand := range lemmaCandidates(word) {
		if o, err := lookupPrimary(client, cand); err == nil && o != "" {
			out, used, source = o, cand, "online"
			break
		}
	}
	if out == "" {
		for _, cand := range lemmaCandidates(word) {
			if o, err := lookupWiktionary(client, cand); err == nil && o != "" {
				out, used, source = o, cand, "wiktionary"
				break
			}
		}
	}

	if out == "" && !cfg.noOffline {
		for _, cand := range lemmaCandidates(word) {
			if o, err := offlineLookup(p, cand); err == nil && o != "" {
				out, used, source = o, cand, "offline"
				break
			}
		}
	}

	if out == "" {
		out, used, source = "No definition found.", word, "none"
	}

	showWord := cap1(word)
	if used != "" && strings.ToLower(word) != used {
		showWord = cap1(word) + " → " + cap1(used)
	}

	full = strings.TrimSpace(out)
	writeLast(full)

	body = "<b><i>" + showWord + "</i></b>\n" + clampBody(full)
	title = "📘 " + cap1(word) + " " + sourceEmoji(source)

	mem.set(key, title, body, full, source)
	disk[key] = diskEntry{Title: title, Body: body, Full: full, TS: time.Now(), Source: source}
	*diskDirty = true

	return title, body, full, source
}

type deduper struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newDeduper() *deduper { return &deduper{last: map[string]time.Time{}} }

func (d *deduper) allow(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if t, ok := d.last[key]; ok && now.Sub(t) < dedupeWindow {
		return false
	}
	d.last[key] = now
	return true
}

func notifyDBusAndHandleClick(p paths, summary, body, full string) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return
	}
	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")

	actions := []string{
		"default", "Open full",
		"full", "Open full",
	}
	hints := map[string]dbus.Variant{
		"resident":  dbus.MakeVariant(true),
		"transient": dbus.MakeVariant(false),
	}
	var id uint32
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		appName, uint32(0), "", summary, body, actions, hints, int32(0),
	)
	if call.Err != nil {
		return
	}
	_ = call.Store(&id)

	c := make(chan *dbus.Signal, 8)
	conn.Signal(c)
	_ = conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.Notifications"),
		dbus.WithMatchMember("ActionInvoked"),
	)

	go func() {
		defer conn.RemoveSignal(c)

		timeout := time.NewTimer(10 * time.Minute)
		defer timeout.Stop()

		for {
			select {
			case sig := <-c:
				if sig == nil || len(sig.Body) < 2 {
					continue
				}
				nid, ok := sig.Body[0].(uint32)
				if !ok || nid != id {
					continue
				}
				action, _ := sig.Body[1].(string)
				if action == "default" || action == "full" {
					openFullText(p, full)
					return
				}
			case <-timeout.C:
				return
			}
		}
	}()
}

func openFullText(p paths, full string) {
	if p.zenity != "" {
		cmd := exec.Command(p.zenity, "--text-info", "--width=760", "--height=560", "--title=define", "--no-markup")
		cmd.Stdin = strings.NewReader(full)
		_ = cmd.Run()
		return
	}
	fmt.Println(full)
}

func openFullFromLast(p paths) {
	b, err := os.ReadFile(lastFilePath())
	if err != nil {
		return
	}
	openFullText(p, string(b))
}

func runDaemon(cfg config, p paths) int {
	sock := runtimeSocketPath()
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	defer ln.Close()
	_ = os.Chmod(sock, 0o600)

	mem := newLRU(memCacheMax, cacheTTL)

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	client := &http.Client{Transport: transport}

	diskPath := cacheFilePath()
	disk := loadDiskCache(diskPath)
	diskDirty := false
	var diskMu sync.Mutex

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			diskMu.Lock()
			if diskDirty {
				saveDiskCacheAtomic(diskPath, disk)
				diskDirty = false
			}
			diskMu.Unlock()
		}
	}()

	ded := newDeduper()

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(900 * time.Millisecond))

			r := bufio.NewReader(c)
			buf := make([]byte, daemonReadMax)
			n, _ := r.Read(buf)
			word := pickWord(string(bytes.TrimSpace(buf[:n])))

			if !validWord(word) {
				return
			}

			key := strings.ToLower(word)
			if !ded.allow(key) {
				return
			}

			diskMu.Lock()
			title, body, full, _ := resolveDefinition(cfg, p, mem, disk, &diskDirty, word, client)
			diskMu.Unlock()

			notifyDBusAndHandleClick(p, title, body, full)
		}(conn)
	}
}

func clientSend(cfg config, word string) error {
	sock := runtimeSocketPath()
	if _, err := os.Stat(sock); err == nil {
		conn, err := net.DialTimeout("unix", sock, 80*time.Millisecond)
		if err == nil {
			_, _ = conn.Write([]byte(word))
			_ = conn.Close()
			return nil
		}
	}
	p := resolvePaths()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	client := &http.Client{Transport: transport}
	mem := newLRU(64, 10*time.Minute)
	disk := loadDiskCache(cacheFilePath())
	dirty := false
	title, body, full, _ := resolveDefinition(cfg, p, mem, disk, &dirty, word, client)
	if dirty {
		saveDiskCacheAtomic(cacheFilePath(), disk)
	}
	notifyDBusAndHandleClick(p, title, body, full)
	return nil
}
