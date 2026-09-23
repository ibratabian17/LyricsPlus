// Command account_manager edits provider and Google Drive credentials in
// auth.json / config.json / .env from a flag-driven CLI or an interactive session.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"lyricsplus/backend/internal/accountmgr"
	"lyricsplus/backend/internal/config"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

// provider order for menus and tables
var providerKinds = []string{"spotify", "apple", "musixmatch", "deezer", "gdrive"}

var providerLabels = map[string]string{
	"spotify":    "Spotify",
	"apple":      "Apple Music",
	"musixmatch": "Musixmatch",
	"deezer":     "Deezer",
	"gdrive":     "Google Drive",
	"qq":         "QQ Music",
}

func main() {
	var (
		filePath    string
		interactive bool
	)

	flag.StringVar(&filePath, "file", "", "Path to auth.json, config.json, or .env (defaults to auto-discovery)")
	flag.StringVar(&filePath, "f", "", "Alias for --file")
	flag.BoolVar(&interactive, "i", false, "Force interactive mode")
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	targetFile := accountmgr.FindConfigFile(filePath)
	store, err := accountmgr.LoadStore(targetFile)
	if err != nil {
		fatal("error loading config %s: %v", targetFile, err)
	}

	ap := &app{store: store, statuses: map[string]accountmgr.TestResult{}}

	args := flag.Args()
	if len(args) == 0 || interactive {
		if isPipe() {
			fatal("interactive mode needs a terminal; use subcommands (e.g. \"account_manager list\")")
		}
		ap.reader = bufio.NewReader(os.Stdin)
		ap.run()
		return
	}

	if err := ap.runCLI(args); err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%v%v\n", colorRed, fmt.Sprintf(format, a...)+colorReset)
	os.Exit(1)
}

func isPipe() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return st.Mode()&os.ModeCharDevice == 0
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `LyricsPlus Account Manager

Usage:
  account_manager [flags]                       Interactive session (auto-saves)
  account_manager list [provider]               List accounts (providers: %s)
  account_manager add <provider> [--field ...]  Add an account from flags
  account_manager edit <provider> <name> [--field ...]
                                                Update any subset of fields
  account_manager remove <provider> <name>      Remove an account
  account_manager test [provider] [name]        Live health-check credentials

Provider fields (add/edit):
  spotify:    --name --cookie --client-id --client-secret
  apple:      --name --auth-type(android|web) --token --dsid --user-agent
              --cookie --storefront
  musixmatch: --name --auth-type(web|android) --cookie --user-agent --email
              --password
  deezer:     --name --refresh-token --arl
  gdrive:     --client-id --client-secret --refresh-token --root
  qq:         --cookie

Flags:
  -f, --file <path>   Specify config auth.json / config.json / .env (auto-discovers)
  -i                  Force interactive mode
`, strings.Join(providerKinds, ", "))
}

// ============================================================================
// Account abstraction
// ============================================================================

type account struct {
	kind    string
	spotify *config.SpotifyAccount
	apple   *config.AppleAccount
	mxm     *config.MusixmatchAccount
	deezer  *config.DeezerAccount
	gdrive  *config.GDriveAccount
}

func (a *account) name() string {
	switch a.kind {
	case "spotify":
		return a.spotify.NAMEID
	case "apple":
		return a.apple.NAMEID
	case "musixmatch":
		return a.mxm.NAMEID
	case "deezer":
		return a.deezer.NAMEID
	case "gdrive":
		return a.gdrive.ClientID
	}
	return ""
}

func (a *account) key() string { return a.kind + "|" + a.name() }

func setName(a *account, v string) {
	switch a.kind {
	case "spotify":
		a.spotify.NAMEID = v
	case "apple":
		a.apple.NAMEID = v
	case "musixmatch":
		a.mxm.NAMEID = v
	case "deezer":
		a.deezer.NAMEID = v
	}
}

type templateField struct {
	key, label string
	secret     bool
	def        func(*account) string // default when adding (nil when none)
	cond       func(*account) bool   // only offered when true (nil = always)
	set        func(*account, string)
}

func templateFields(kind string) []templateField {
	switch kind {
	case "spotify":
		return []templateField{
			{key: "name", label: "Name/ID", set: func(a *account, v string) { a.spotify.NAMEID = v }},
			{key: "cookie", label: "sp_dc cookie or full Cookie header", secret: true, set: func(a *account, v string) { a.spotify.COOKIE = accountmgr.ExtractSpDc(v) }},
			{key: "client-id", label: "Client ID", secret: true, set: func(a *account, v string) { a.spotify.CLIENT_ID = v }},
			{key: "client-secret", label: "Client Secret", secret: true, set: func(a *account, v string) { a.spotify.CLIENT_SECRET = v }},
		}
	case "apple":
		isAndroid := func(a *account) bool { return !strings.EqualFold(a.apple.AUTH_TYPE, "web") }
		return []templateField{
			{key: "auth-type", label: "Auth type (android/web)", set: func(a *account, v string) {
				a.apple.AUTH_TYPE = strings.ToLower(strings.TrimSpace(v))
			}},
			{key: "name", label: "Name/ID", set: func(a *account, v string) { a.apple.NAMEID = v }},
			{key: "storefront", label: "Storefront (e.g. us, in)", def: func(*account) string { return "us" }, set: func(a *account, v string) { a.apple.STOREFRONT = strings.ToLower(strings.TrimSpace(v)) }},
			{key: "token", label: "Auth token", secret: true, set: func(a *account, v string) {
				if isAndroid(a) {
					a.apple.ANDROID_AUTH_TOKEN = v
				} else {
					a.apple.MUSIC_AUTH_TOKEN = v
				}
			}},
			{key: "dsid", label: "Android DSID", secret: true, cond: isAndroid, set: func(a *account, v string) { a.apple.ANDROID_DSID = v }},
			{key: "user-agent", label: "Android User-Agent", cond: isAndroid, set: func(a *account, v string) { a.apple.ANDROID_USER_AGENT = v }},
			{key: "cookie", label: "Android Cookie", secret: true, cond: isAndroid, set: func(a *account, v string) { a.apple.ANDROID_COOKIE = v }},
		}
	case "musixmatch":
		isAndroid := func(a *account) bool { return strings.EqualFold(a.mxm.AUTH_TYPE, "android") }
		return []templateField{
			{key: "auth-type", label: "Auth type (web/android)", set: func(a *account, v string) {
				a.mxm.AUTH_TYPE = strings.ToLower(strings.TrimSpace(v))
			}},
			{key: "name", label: "Name/ID", set: func(a *account, v string) { a.mxm.NAMEID = v }},
			{key: "cookie", label: "Musixmatch cookie", secret: true, cond: func(a *account) bool { return !isAndroid(a) }, set: func(a *account, v string) { a.mxm.COOKIE = accountmgr.CleanCookie(v) }},
			{key: "user-agent", label: "User-Agent", cond: func(a *account) bool { return !isAndroid(a) }, set: func(a *account, v string) { a.mxm.USER_AGENT = v }},
			{key: "email", label: "Account email", cond: isAndroid, set: func(a *account, v string) { a.mxm.EMAIL = v }},
			{key: "password", label: "Password", secret: true, cond: isAndroid, set: func(a *account, v string) { a.mxm.PASSWORD = v }},
		}
	case "deezer":
		return []templateField{
			{key: "name", label: "Name/ID", set: func(a *account, v string) { a.deezer.NAMEID = v }},
			{key: "refresh-token", label: "Refresh Token", secret: true, set: func(a *account, v string) { a.deezer.REFRESH_TOKEN = strings.TrimSpace(v) }},
			{key: "arl", label: "ARL", secret: true, set: func(a *account, v string) { a.deezer.ARL = strings.TrimSpace(v) }},
		}
	case "gdrive":
		return []templateField{
			{key: "client-id", label: "OAuth Client ID", secret: true, set: func(a *account, v string) { a.gdrive.ClientID = strings.TrimSpace(v) }},
			{key: "client-secret", label: "OAuth Client Secret", secret: true, set: func(a *account, v string) { a.gdrive.ClientSecret = strings.TrimSpace(v) }},
			{key: "refresh-token", label: "OAuth Refresh Token", secret: true, set: func(a *account, v string) { a.gdrive.RefreshToken = strings.TrimSpace(v) }},
			{key: "root", label: "Root Folder ID", set: func(a *account, v string) { a.gdrive.Root = strings.TrimSpace(v) }},
		}
	}
	return nil
}

func (a *account) hasCredential() bool {
	switch a.kind {
	case "spotify":
		return a.spotify.COOKIE != "" || (a.spotify.CLIENT_ID != "" && a.spotify.CLIENT_SECRET != "")
	case "apple":
		if strings.EqualFold(a.apple.AUTH_TYPE, "web") {
			return a.apple.MUSIC_AUTH_TOKEN != ""
		}
		return a.apple.ANDROID_AUTH_TOKEN != ""
	case "musixmatch":
		return a.mxm.COOKIE != "" || (a.mxm.EMAIL != "" && a.mxm.PASSWORD != "")
	case "deezer":
		return a.deezer.REFRESH_TOKEN != "" || a.deezer.ARL != ""
	case "gdrive":
		return a.gdrive.ClientID != "" && a.gdrive.RefreshToken != ""
	}
	return false
}

func (a *account) summary() string {
	switch a.kind {
	case "spotify":
		desc := "cookie: " + accountmgr.MaskCredential(a.spotify.COOKIE)
		if a.spotify.CLIENT_ID != "" {
			desc += ", client: " + accountmgr.MaskCredential(a.spotify.CLIENT_ID)
		}
		return desc
	case "apple":
		atype := a.apple.AUTH_TYPE
		if atype == "" {
			atype = "android"
		}
		tok := a.apple.ANDROID_AUTH_TOKEN
		if strings.EqualFold(atype, "web") {
			tok = a.apple.MUSIC_AUTH_TOKEN
		}
		sf := a.apple.STOREFRONT
		if sf == "" {
			sf = "us"
		}
		return fmt.Sprintf("%s, sf: %s, token: %s", atype, sf, accountmgr.MaskCredential(tok))
	case "musixmatch":
		if strings.EqualFold(a.mxm.AUTH_TYPE, "android") {
			return "android, email: " + emptyOr(a.mxm.EMAIL, "<none>")
		}
		return "web, cookie: " + accountmgr.MaskCredential(a.mxm.COOKIE)
	case "deezer":
		if a.deezer.REFRESH_TOKEN != "" {
			return "refresh-token: " + accountmgr.MaskCredential(a.deezer.REFRESH_TOKEN)
		}
		return "arl: " + accountmgr.MaskCredential(a.deezer.ARL)
	case "gdrive":
		return fmt.Sprintf("root: %s, client: %s", emptyOr(a.gdrive.Root, "<default>"), accountmgr.MaskCredential(a.gdrive.ClientID))
	}
	return ""
}

func defaultName(kind string, store *accountmgr.Store) string {
	n := 1
	switch kind {
	case "spotify":
		n = len(store.SpotifyAccounts)
	case "apple":
		n = len(store.AppleAccounts)
	case "musixmatch":
		n = len(store.MusixmatchAccounts)
	case "deezer":
		n = len(store.DeezerAccounts)
	}
	return fmt.Sprintf("%s-%d", kind, n)
}

func accountsOf(store *accountmgr.Store, kind string) []*account {
	switch kind {
	case "spotify":
		out := make([]*account, 0, len(store.SpotifyAccounts))
		for i := range store.SpotifyAccounts {
			out = append(out, &account{kind: "spotify", spotify: &store.SpotifyAccounts[i]})
		}
		return out
	case "apple":
		out := make([]*account, 0, len(store.AppleAccounts))
		for i := range store.AppleAccounts {
			out = append(out, &account{kind: "apple", apple: &store.AppleAccounts[i]})
		}
		return out
	case "musixmatch":
		out := make([]*account, 0, len(store.MusixmatchAccounts))
		for i := range store.MusixmatchAccounts {
			out = append(out, &account{kind: "musixmatch", mxm: &store.MusixmatchAccounts[i]})
		}
		return out
	case "deezer":
		out := make([]*account, 0, len(store.DeezerAccounts))
		for i := range store.DeezerAccounts {
			out = append(out, &account{kind: "deezer", deezer: &store.DeezerAccounts[i]})
		}
		return out
	case "gdrive":
		out := make([]*account, 0, len(store.GDriveAccounts))
		for i := range store.GDriveAccounts {
			out = append(out, &account{kind: "gdrive", gdrive: &store.GDriveAccounts[i]})
		}
		return out
	}
	return nil
}

func allAccounts(store *accountmgr.Store) []*account {
	var out []*account
	for _, k := range providerKinds {
		out = append(out, accountsOf(store, k)...)
	}
	return out
}

func findAccount(store *accountmgr.Store, kind, name string) *account {
	for _, a := range accountsOf(store, kind) {
		if strings.EqualFold(a.name(), name) {
			return a
		}
	}
	return nil
}

func appendAccount(store *accountmgr.Store, kind string) *account {
	switch kind {
	case "spotify":
		store.SpotifyAccounts = append(store.SpotifyAccounts, config.SpotifyAccount{})
		return &account{kind: "spotify", spotify: &store.SpotifyAccounts[len(store.SpotifyAccounts)-1]}
	case "apple":
		store.AppleAccounts = append(store.AppleAccounts, config.AppleAccount{AUTH_TYPE: "android"})
		return &account{kind: "apple", apple: &store.AppleAccounts[len(store.AppleAccounts)-1]}
	case "musixmatch":
		store.MusixmatchAccounts = append(store.MusixmatchAccounts, config.MusixmatchAccount{AUTH_TYPE: "web"})
		return &account{kind: "musixmatch", mxm: &store.MusixmatchAccounts[len(store.MusixmatchAccounts)-1]}
	case "deezer":
		store.DeezerAccounts = append(store.DeezerAccounts, config.DeezerAccount{})
		return &account{kind: "deezer", deezer: &store.DeezerAccounts[len(store.DeezerAccounts)-1]}
	case "gdrive":
		store.GDriveAccounts = append(store.GDriveAccounts, config.GDriveAccount{})
		return &account{kind: "gdrive", gdrive: &store.GDriveAccounts[len(store.GDriveAccounts)-1]}
	}
	return nil
}

func removeLast(store *accountmgr.Store, kind string) {
	switch kind {
	case "spotify":
		if n := len(store.SpotifyAccounts); n > 0 {
			store.SpotifyAccounts = store.SpotifyAccounts[:n-1]
		}
	case "apple":
		if n := len(store.AppleAccounts); n > 0 {
			store.AppleAccounts = store.AppleAccounts[:n-1]
		}
	case "musixmatch":
		if n := len(store.MusixmatchAccounts); n > 0 {
			store.MusixmatchAccounts = store.MusixmatchAccounts[:n-1]
		}
	case "deezer":
		if n := len(store.DeezerAccounts); n > 0 {
			store.DeezerAccounts = store.DeezerAccounts[:n-1]
		}
	case "gdrive":
		if n := len(store.GDriveAccounts); n > 0 {
			store.GDriveAccounts = store.GDriveAccounts[:n-1]
		}
	}
}

func removeByName(store *accountmgr.Store, kind, name string) bool {
	for i, a := range accountsOf(store, kind) {
		if !strings.EqualFold(a.name(), name) {
			continue
		}
		switch kind {
		case "spotify":
			store.SpotifyAccounts = append(store.SpotifyAccounts[:i], store.SpotifyAccounts[i+1:]...)
		case "apple":
			store.AppleAccounts = append(store.AppleAccounts[:i], store.AppleAccounts[i+1:]...)
		case "musixmatch":
			store.MusixmatchAccounts = append(store.MusixmatchAccounts[:i], store.MusixmatchAccounts[i+1:]...)
		case "deezer":
			store.DeezerAccounts = append(store.DeezerAccounts[:i], store.DeezerAccounts[i+1:]...)
		case "gdrive":
			store.GDriveAccounts = append(store.GDriveAccounts[:i], store.GDriveAccounts[i+1:]...)
		}
		return true
	}
	return false
}

func emptyOr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ============================================================================
// App: shared state + table output
// ============================================================================

type app struct {
	store    *accountmgr.Store
	reader   *bufio.Reader
	statuses map[string]accountmgr.TestResult // live test results (interactive)
}

func (ap *app) header() {
	ap.printBanner()
	ap.listTable("")
}

func sep() string {
	return strings.Repeat("-", terminalWidth())
}

func terminalWidth() int {
	for _, fd := range []int{1, 0} {
		if ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
			return int(ws.Col)
		}
	}
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func stripColor(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		if r == 0x1b {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (ap *app) printBanner() {
	fmt.Printf("\n%s%sLyricsPlus Account Manager%s\n", colorBold, colorPurple, colorReset)
	fmt.Printf("File: %s  (changes are saved automatically)\n", colorYellow+ap.store.FilePath()+colorReset)
	fmt.Println(sep())
}

func (ap *app) listTable(provider string) {
	type row struct {
		idx          int
		prov, name   string
		cred, status string
	}

	var rows []row
	idx := 1
	for _, kind := range providerKinds {
		if provider != "" && provider != kind {
			continue
		}
		for _, a := range accountsOf(ap.store, kind) {
			rows = append(rows, row{idx, providerLabels[kind], a.name(), a.summary(), ap.statusCol(a.key())})
			idx++
		}
	}
	if provider == "" || provider == "qq" {
		if ap.store.QQCookie != "" {
			rows = append(rows, row{idx, providerLabels["qq"], providerLabels["qq"], "cookie: " + accountmgr.MaskCredential(ap.store.QQCookie), "-"})
		}
	}

	if len(rows) == 0 {
		if provider == "" {
			fmt.Println("(no accounts configured yet — use \"add\" to create one)")
		} else {
			fmt.Printf("%sno %s accounts configured%s\n", colorYellow, providerLabels[provider], colorReset)
		}
		return
	}

	numW, provW, nameW, statusW := 1, len("Provider"), 1, len("Status")
	for _, r := range rows {
		numW = max(numW, len(strconv.Itoa(r.idx)))
		provW = max(provW, len(r.prov))
		nameW = max(nameW, len(r.name))
		statusW = max(statusW, len(stripColor(r.status)))
	}
	nameW = min(nameW, 28)
	statusW = min(statusW, 26)
	pad := 2
	credW := terminalWidth() - numW - provW - nameW - statusW - pad*4
	if credW < 24 {
		credW = 24
	}

	gap := strings.Repeat(" ", pad)
	fmt.Fprintf(os.Stdout, "%s%s%s%s%s%s%s%s%s\n",
		padRight("#", numW), gap,
		padRight("Provider", provW), gap,
		padRight("Name", nameW), gap,
		padRight("Credential", credW), gap,
		padRight("Status", statusW))
	for _, r := range rows {
		stl := truncate(stripColor(r.status), statusW)
		fmt.Fprintf(os.Stdout, "%s%s%s%s%s%s%s%s%s%s\n",
			padRight(strconv.Itoa(r.idx), numW), gap,
			padRight(r.prov, provW), gap,
			padRight(truncate(r.name, nameW), nameW), gap,
			padRight(truncate(r.cred, credW), credW), gap,
			r.status, strings.Repeat(" ", max(0, statusW-len(stl))))
	}
}

func (ap *app) statusCol(key string) string {
	res, ok := ap.statuses[key]
	if !ok {
		return "—"
	}
	color := colorGreen
	switch res.Status {
	case "ERROR", "UNAUTHORIZED":
		color = colorRed
	case "SKIPPED", "EXPIRED":
		color = colorYellow
	}
	return fmt.Sprintf("%s%s (%dms)%s", color, res.Status, res.Duration.Milliseconds(), colorReset)
}

func (ap *app) commit(msg string) error {
	if err := ap.store.Save(); err != nil {
		return fmt.Errorf("error saving to %s: %w", ap.store.FilePath(), err)
	}
	fmt.Printf("%s%s%s\n", colorGreen, msg, colorReset)
	return nil
}

// ============================================================================
// Interactive session
// ============================================================================

func (ap *app) run() {
	for {
		ap.header()
		fmt.Println(sep())
		action := ap.prompt("Action (a add, e edit, r remove, t test, l list, q quit)", "q")
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "a", "add":
			ap.addMenu()
		case "e", "edit":
			ap.editMenu()
		case "r", "rm", "remove":
			ap.removeMenu()
		case "t", "test":
			ap.testMenu()
		case "l", "list":
			ap.pause()
		case "", "q", "quit", "exit":
			return
		default:
			fmt.Println("Unknown action.")
		}
	}
}

func (ap *app) addMenu() {
	fmt.Println()
	for i, k := range providerKinds {
		fmt.Printf("  %d) %s\n", i+1, providerLabels[k])
	}
	fmt.Printf("  %d) QQ Music cookie\n", len(providerKinds)+1)

	n, err := strconv.Atoi(ap.prompt("Provider", ""))
	if err != nil || n < 1 || n > len(providerKinds)+1 {
		fmt.Println("Invalid provider.")
		return
	}
	if n == len(providerKinds)+1 {
		c := ap.prompt("New QQ Music cookie (Enter stays empty)", "")
		if c == "" {
			return
		}
		ap.store.QQCookie = accountmgr.CleanCookie(c)
		_ = ap.commit("QQ Music cookie updated.")
		return
	}

	kind := providerKinds[n-1]
	a := appendAccount(ap.store, kind)
	if !ap.wizardAdd(a) {
		removeLast(ap.store, kind)
		return
	}
	if !a.hasCredential() {
		removeLast(ap.store, kind)
		fmt.Printf("%sDiscarded: at least one credential is required.%s\n", colorYellow, colorReset)
		return
	}
	_ = ap.commit(fmt.Sprintf("Added %s account %q.", providerLabels[kind], a.name()))
}

func (ap *app) wizardAdd(a *account) bool {
	if a.kind != "gdrive" {
		name := ap.prompt("Name/ID", defaultName(a.kind, ap.store))
		if strings.TrimSpace(name) == "" {
			return false
		}
		setName(a, name)
	}

	for _, tf := range templateFields(a.kind) {
		if tf.key == "name" || (tf.cond != nil && !tf.cond(a)) {
			continue
		}
		def := ""
		if tf.def != nil {
			def = tf.def(a)
		}
		v := ap.prompt(tf.label, def)
		tf.set(a, v)
	}
	return true
}

func (ap *app) editMenu() {
	rows := allAccounts(ap.store)
	if len(rows) == 0 {
		fmt.Println("No accounts to edit.")
		return
	}
	ap.listTable("")

	i, err := strconv.Atoi(ap.prompt("Row to edit", ""))
	if err != nil || i < 1 || i > len(rows) {
		fmt.Println("Invalid row.")
		return
	}
	a := rows[i-1]

	for {
		fields := templateFields(a.kind)
		fmt.Printf("\nEditing %s account %q:\n", providerLabels[a.kind], a.name())
		for n, tf := range fields {
			if tf.cond != nil && !tf.cond(a) {
				continue
			}
			cur := currentValue(a, tf.key)
			if tf.secret && cur != "" {
				cur = accountmgr.MaskCredential(cur)
			}
			fmt.Printf("  [%d] %-22s %s\n", n+1, tf.label, cur)
		}

		f, err := strconv.Atoi(ap.prompt("Field to change (Enter = done)", ""))
		if err != nil || f < 1 || f > len(fields) {
			return
		}
		tf := fields[f-1]
		cur := currentValue(a, tf.key)
		if tf.secret && cur != "" {
			fmt.Printf("(current value: %s)\n", accountmgr.MaskCredential(cur))
		} else if cur != "" {
			fmt.Printf("(current value: %s)\n", cur)
		}
		v := ap.prompt("New value (Enter keeps current)", "")
		if v == "" {
			continue
		}
		tf.set(a, v)
		_ = ap.commit(fmt.Sprintf("Updated %s account %q.", providerLabels[a.kind], a.name()))
	}
}

func (ap *app) removeMenu() {
	rows := allAccounts(ap.store)
	if len(rows) == 0 {
		fmt.Println("No accounts to remove.")
		return
	}
	ap.listTable("")

	i, err := strconv.Atoi(ap.prompt("Row to remove", ""))
	if err != nil || i < 1 || i > len(rows) {
		fmt.Println("Invalid row.")
		return
	}
	a := rows[i-1]

	yes := ap.prompt(fmt.Sprintf("Remove %s account %q? (y/N)", providerLabels[a.kind], a.name()), "n")
	if !strings.EqualFold(yes, "y") && !strings.EqualFold(yes, "yes") {
		fmt.Println("Cancelled.")
		return
	}
	if removeByName(ap.store, a.kind, a.name()) {
		delete(ap.statuses, a.key())
		_ = ap.commit(fmt.Sprintf("Removed %s account %q.", providerLabels[a.kind], a.name()))
	}
}

func (ap *app) testMenu() {
	provider := ap.prompt("Provider to test (Enter = all)", "")
	ap.test(strings.TrimSpace(provider), "")
}

// ============================================================================
// Live credential testing (shared by interactive + CLI)
// ============================================================================

func (ap *app) test(provider, name string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	tester := accountmgr.NewTester()
	ctx := context.Background()

	if provider == "qq" {
		fmt.Printf("QQ Music: cookie %s — no live test available.\n", accountmgr.MaskCredential(ap.store.QQCookie))
		return
	}
	if provider != "" && providerLabels[provider] == "" {
		fmt.Printf("%sunknown provider %q%s\n", colorRed, provider, colorReset)
		return
	}

	type testRow struct {
		idx            int
		prov, name     string
		status, detail string
	}
	var trows []testRow
	tested := 0
	idx := 1
	for _, kind := range providerKinds {
		if provider != "" && provider != kind {
			continue
		}
		for _, a := range accountsOf(ap.store, kind) {
			if name != "" && !strings.EqualFold(a.name(), name) {
				continue
			}
			var res accountmgr.TestResult
			switch kind {
			case "spotify":
				res = tester.TestSpotify(ctx, *a.spotify)
			case "apple":
				res = tester.TestApple(ctx, *a.apple)
			case "musixmatch":
				res = tester.TestMusixmatch(ctx, *a.mxm)
			case "deezer":
				res = tester.TestDeezer(ctx, *a.deezer)
			case "gdrive":
				res = tester.TestGDrive(ctx, *a.gdrive)
			}
			ap.statuses[a.key()] = res
			color := colorGreen
			switch res.Status {
			case "ERROR", "UNAUTHORIZED":
				color = colorRed
			case "SKIPPED", "EXPIRED":
				color = colorYellow
			}
			trows = append(trows, testRow{idx, providerLabels[kind], a.name(),
				color + res.Status + colorReset, strings.ReplaceAll(res.Message, "\n", " ")})
			idx++
			tested++
		}
	}

	if tested > 0 {
		numW, provW, nameW, statusW := 1, len("Provider"), 1, len("Status")
		for _, r := range trows {
			numW = max(numW, len(strconv.Itoa(r.idx)))
			provW = max(provW, len(r.prov))
			nameW = max(nameW, len(r.name))
			statusW = max(statusW, len(stripColor(r.status)))
		}
		nameW = min(nameW, 28)
		statusW = min(statusW, 26)
		pad := 2
		gap := strings.Repeat(" ", pad)
		detailW := terminalWidth() - numW - provW - nameW - statusW - pad*4
		if detailW < 24 {
			detailW = 24
		}

		fmt.Printf("%s%s%s%s%s%s%s%s%s\n",
			padRight("#", numW), gap,
			padRight("Provider", provW), gap,
			padRight("Name", nameW), gap,
			padRight("Status", statusW), gap,
			padRight("Detail", detailW))
		for _, r := range trows {
			stl := truncate(stripColor(r.status), statusW)
			fmt.Printf("%s%s%s%s%s%s%s%s%s%s\n",
				padRight(strconv.Itoa(r.idx), numW), gap,
				padRight(r.prov, provW), gap,
				padRight(truncate(r.name, nameW), nameW), gap,
				padRight(stl, statusW), gap,
				r.status, padRight(truncate(r.detail, detailW), detailW))
		}
	}

	if tested == 0 {
		fmt.Println("No accounts matched.")
	}
	fmt.Println()
}

// ============================================================================
// CLI subcommands (flag-driven, no hidden prompts)
// ============================================================================

func (ap *app) runCLI(args []string) error {
	cmd := strings.ToLower(args[0])
	switch cmd {
	case "list", "ls":
		provider := ""
		if len(args) > 1 {
			provider = strings.ToLower(args[1])
		}
		if provider != "" && providerLabels[provider] == "" {
			return fmt.Errorf("unknown provider %q (valid: %s)", provider, strings.Join(providerKinds, ", "))
		}
		ap.printBanner()
		ap.listTable(provider)
		return nil

	case "add":
		if len(args) < 2 {
			return errUsage("add <provider> [--field ...]")
		}
		kv, err := parseKVs(args[2:])
		if err != nil {
			return err
		}
		return ap.addFromFlags(strings.ToLower(args[1]), kv)

	case "edit":
		if len(args) < 3 {
			return errUsage("edit <provider> <name> [--field ...]")
		}
		kv, err := parseKVs(args[3:])
		if err != nil {
			return err
		}
		return ap.editFromFlags(strings.ToLower(args[1]), args[2], kv)

	case "remove", "rm", "delete":
		if len(args) < 3 {
			return errUsage("remove <provider> <name>")
		}
		return ap.removeFromFlags(strings.ToLower(args[1]), args[2])

	case "test":
		provider := ""
		if len(args) > 1 {
			provider = strings.ToLower(args[1])
		}
		name := ""
		if len(args) > 2 {
			name = args[2]
		}
		ap.test(provider, name)
		return nil

	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return nil

	default:
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func errUsage(usage string) error { return fmt.Errorf("usage: account_manager %s", usage) }

func parseKVs(args []string) (map[string]string, error) {
	kv := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("unexpected argument %q (fields use --key value)", arg)
		}
		raw := strings.TrimPrefix(arg, "--")
		key, val, hasVal := strings.Cut(raw, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, fmt.Errorf("empty field name in %q", arg)
		}
		if !hasVal {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("missing value for --%s", key)
			}
			val = args[i]
		}
		kv[key] = val
	}
	return kv, nil
}

func (ap *app) setters(kind string) map[string]func(*account, string) {
	m := map[string]func(*account, string){}
	for _, tf := range templateFields(kind) {
		m[tf.key] = tf.set
	}
	return m
}

func (ap *app) checkKeys(kind string, kv map[string]string) error {
	set := ap.setters(kind)
	var unknown []string
	for k := range kv {
		if k == "name" || set[k] != nil {
			continue
		}
		unknown = append(unknown, k)
	}
	if len(unknown) > 0 {
		var keys []string
		for _, tf := range templateFields(kind) {
			keys = append(keys, tf.key)
		}
		return fmt.Errorf("unknown field(s) for %s: --%s (valid: --%s)", providerLabels[kind], strings.Join(unknown, ", --"), strings.Join(keys, ", --"))
	}
	return nil
}

func (ap *app) addFromFlags(kind string, kv map[string]string) error {
	if kind == "qq" {
		c := kv["cookie"]
		if c == "" {
			return fmt.Errorf("setting the QQ Music cookie requires --cookie")
		}
		ap.store.QQCookie = accountmgr.CleanCookie(c)
		return ap.commit("QQ Music cookie updated.")
	}
	if providerLabels[kind] == "" {
		return fmt.Errorf("unknown provider %q (valid: %s)", kind, strings.Join(providerKinds, ", "))
	}
	if len(kv) == 0 {
		return fmt.Errorf("add %s requires at least one --field (e.g. --cookie, --name); run \"account_manager help\" for the field list", kind)
	}
	if err := ap.checkKeys(kind, kv); err != nil {
		return err
	}

	a := appendAccount(ap.store, kind)
	setters := ap.setters(kind)
	if t, ok := kv["auth-type"]; ok {
		if set := setters["auth-type"]; set != nil {
			set(a, t)
		}
	}
	for k, v := range kv {
		if k == "auth-type" {
			continue
		}
		if set := setters[k]; set != nil {
			set(a, v)
		}
	}

	if a.kind != "gdrive" && strings.TrimSpace(a.name()) == "" {
		setName(a, defaultName(kind, ap.store))
	}
	if !a.hasCredential() {
		removeLast(ap.store, kind)
		return fmt.Errorf("no usable credentials provided for the new %s account", providerLabels[kind])
	}
	return ap.commit(fmt.Sprintf("Added %s account %q.", providerLabels[kind], a.name()))
}

func (ap *app) editFromFlags(kind, name string, kv map[string]string) error {
	if kind == "qq" {
		return fmt.Errorf("QQ Music has no account rows; use \"add qq --cookie ...\" instead")
	}
	if providerLabels[kind] == "" {
		return fmt.Errorf("unknown provider %q (valid: %s)", kind, strings.Join(providerKinds, ", "))
	}
	if len(kv) == 0 {
		return fmt.Errorf("edit needs at least one --field to change (e.g. --cookie \"new value\")")
	}
	if err := ap.checkKeys(kind, kv); err != nil {
		return err
	}
	a := findAccount(ap.store, kind, name)
	if a == nil {
		return fmt.Errorf("no %s account named %q", providerLabels[kind], name)
	}
	setters := ap.setters(kind)
	if t, ok := kv["auth-type"]; ok {
		if set := setters["auth-type"]; set != nil {
			set(a, t)
		}
	}
	for k, v := range kv {
		if k == "auth-type" {
			continue
		}
		if set := setters[k]; set != nil {
			set(a, v)
		}
	}
	delete(ap.statuses, a.key())
	return ap.commit(fmt.Sprintf("Updated %s account %q.", providerLabels[kind], a.name()))
}

func (ap *app) removeFromFlags(kind, name string) error {
	if kind == "qq" {
		return fmt.Errorf("QQ Music has no account rows; use \"add qq --cookie ...\" instead")
	}
	if providerLabels[kind] == "" {
		return fmt.Errorf("unknown provider %q (valid: %s)", kind, strings.Join(providerKinds, ", "))
	}
	if !removeByName(ap.store, kind, name) {
		return fmt.Errorf("no %s account named %q", providerLabels[kind], name)
	}
	ap.statuses = map[string]accountmgr.TestResult{}
	return ap.commit(fmt.Sprintf("Removed %s account %q.", providerLabels[kind], name))
}

// ============================================================================
// Prompt helpers
// ============================================================================

func (ap *app) prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s%s [%s]: %s", colorCyan, label, def, colorReset)
	} else {
		fmt.Printf("%s%s: %s", colorCyan, label, colorReset)
	}
	line, _ := ap.reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func (ap *app) pause() {
	fmt.Print("\nPress Enter to continue...")
	_, _ = ap.reader.ReadString('\n')
}

func currentValue(a *account, key string) string {
	switch a.kind {
	case "spotify":
		switch key {
		case "name":
			return a.spotify.NAMEID
		case "cookie":
			return a.spotify.COOKIE
		case "client-id":
			return a.spotify.CLIENT_ID
		case "client-secret":
			return a.spotify.CLIENT_SECRET
		}
	case "apple":
		switch key {
		case "name":
			return a.apple.NAMEID
		case "auth-type":
			return a.apple.AUTH_TYPE
		case "storefront":
			return a.apple.STOREFRONT
		case "token":
			if strings.EqualFold(a.apple.AUTH_TYPE, "web") {
				return a.apple.MUSIC_AUTH_TOKEN
			}
			return a.apple.ANDROID_AUTH_TOKEN
		case "dsid":
			return a.apple.ANDROID_DSID
		case "user-agent":
			return a.apple.ANDROID_USER_AGENT
		case "cookie":
			return a.apple.ANDROID_COOKIE
		}
	case "musixmatch":
		switch key {
		case "name":
			return a.mxm.NAMEID
		case "auth-type":
			return a.mxm.AUTH_TYPE
		case "cookie":
			return a.mxm.COOKIE
		case "user-agent":
			return a.mxm.USER_AGENT
		case "email":
			return a.mxm.EMAIL
		case "password":
			return a.mxm.PASSWORD
		}
	case "deezer":
		switch key {
		case "name":
			return a.deezer.NAMEID
		case "refresh-token":
			return a.deezer.REFRESH_TOKEN
		case "arl":
			return a.deezer.ARL
		}
	case "gdrive":
		switch key {
		case "client-id":
			return a.gdrive.ClientID
		case "client-secret":
			return a.gdrive.ClientSecret
		case "refresh-token":
			return a.gdrive.RefreshToken
		case "root":
			return a.gdrive.Root
		}
	}
	return ""
}
