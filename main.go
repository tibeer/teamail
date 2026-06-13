package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"bytes"
	"crypto/tls"
	"slices"
	"strconv"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
)

type Config struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`

	Mailboxes []MailboxConfig `yaml:"mailboxes"`
	Users     []UserConfig    `yaml:"users"`
}

type MailboxConfig struct {
	Name     string `yaml:"name"`
	Server   string `yaml:"server"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type UserConfig struct {
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
	Mailboxes []string `yaml:"mailboxes"`
}

type Session struct {
	Username string
	Created  time.Time
}

type App struct {
	cfg      Config
	sessions map[string]Session
	mu       sync.RWMutex
}

type Folder struct {
	Name string
}

type MailSummary struct {
	UID     uint32
	From    string
	Subject string
	Date    string
}

type MessageView struct {
	Account string
	Folder  string
	UID     uint32
	Subject string
	From    string
	Date    string
	Text    string
	HTML    template.HTML
}

type ParsedMessage struct {
	Subject string
	From    string
	Date    string
	Text    string
	HTML    string
	Raw     string
}

var funcMap = template.FuncMap{
	"urlquery": url.QueryEscape,
}

var loginTemplate = template.Must(template.New("login").Parse(`
<!DOCTYPE html>
<html>
<head>
<title>Mail Viewer</title>
<style>
body{
font-family:sans-serif;
max-width:500px;
margin:50px auto;
}
input{
width:100%;
padding:8px;
margin:5px 0;
}
button{
padding:8px 15px;
}
.error{
color:red;
}
</style>
</head>
<body>
<h2>Mail Viewer Login</h2>
{{if .Error}}
<p class="error">{{.Error}}</p>
{{end}}
<form method="POST" action="/login">
<label>Username</label>
<input name="username">
<label>Password</label>
<input type="password" name="password">
<button type="submit">Login</button>
</form>
</body>
</html>
`))

var foldersTemplate = template.Must(template.New("folders").Parse(`
<!DOCTYPE html>
<html>
<head>
<title>{{.Account}}</title>
</head>
<body>
<a href="/">← Accounts</a>
<h2>{{.Account}} folders</h2>
<ul>
{{range .Folders}}
<li>
<a href="/mailbox/{{$.Account}}/{{.Name | urlquery}}">{{.Name}}</a>
</li>
{{end}}
</ul>
</body>
</html>
`))

var mailboxesTemplate = template.Must(template.New("boxes").Parse(`
<!DOCTYPE html>
<html>
<body>
<h2>Available Mailboxes</h2>
<p>Logged in as <b>{{.Username}}</b></p>
<form action="/logout" method="POST">
<button type="submit">Logout</button>
</form>
<ul>
{{range .Accounts}}
<li>
<a href="/mailbox/{{.}}">{{.}}</a>
</li>
{{end}}
</ul>
</body>
</html>
`))

var inboxTemplate = template.Must(template.New("inbox").Funcs(funcMap).Parse(`
<!DOCTYPE html>
<html>
<head>
<title>{{.Mailbox}}</title>
<style>
body{
font-family:sans-serif;
margin:20px;
}
table{
width:100%;
border-collapse:collapse;
}
td,th{
border:1px solid #ddd;
padding:8px;
}
</style>
</head>
<body>
<a href="/mailbox/{{.Account}}">← Back to folders</a>
<h2>{{.Mailbox}}</h2>
<table>
<tr>
<th>Date</th>
<th>From</th>
<th>Subject</th>
</tr>
{{range .Messages}}
<tr>
<td>{{.Date}}</td>
<td>{{.From}}</td>
<td>
<a href="/mailbox/{{$.Account}}/{{$.Folder}}/{{.UID}}">
{{.Subject}}
</a>
</td>
</tr>
{{end}}
</table>
</body>
</html>
`))

var messageTemplate = template.Must(template.New("message").Parse(`
<!DOCTYPE html>
<html>
<head>
<title>{{.Subject}}</title>
<style>
body{
font-family:sans-serif;
margin:20px;
}
.meta{
background:#f0f0f0;
padding:10px;
margin-bottom:20px;
}
.content{
border:1px solid #ddd;
padding:15px;
}
pre{
white-space:pre-wrap;
}
</style>
</head>
<body>
<a href="/mailbox/{{.Account}}/{{.Folder}}">← Back to folder</a>
 |
<a href="/mailbox/{{.Account}}/{{.Folder}}/{{.UID}}/raw">Raw Message</a>
<div class="meta">
<b>Subject:</b> {{.Subject}}<br>
<b>From:</b> {{.From}}<br>
<b>Date:</b> {{.Date}}
</div>
<div class="content">
{{if .HTML}}
{{.HTML}}
{{else}}
<pre>{{.Text}}</pre>
{{end}}
</div>
</body>
</html>
`))

func loadConfig(path string) (Config, error) {
	var cfg Config

	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	err = yaml.Unmarshal(b, &cfg)

	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}

	return cfg, err
}

func (a *App) createSession(username string) string {
	buf := make([]byte, 32)

	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}

	token := hex.EncodeToString(buf)

	a.mu.Lock()
	a.sessions[token] = Session{
		Username: username,
		Created:  time.Now(),
	}
	a.mu.Unlock()

	return token
}

func (a *App) getSession(token string) (Session, bool) {
	a.mu.RLock()
	s, ok := a.sessions[token]
	a.mu.RUnlock()

	if !ok {
		return Session{}, false
	}

	// session still valid
	if time.Since(s.Created) <= 24*time.Hour {
		return s, true
	}

	// expired → upgrade to write lock and delete
	a.mu.Lock()
	defer a.mu.Unlock()

	// re-check in case another goroutine already deleted it
	s2, ok := a.sessions[token]
	if ok && time.Since(s2.Created) > 24*time.Hour {
		delete(a.sessions, token)
	}

	return Session{}, false
}

func (a *App) deleteSession(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func (a *App) findUser(username string) (*UserConfig, error) {
	for _, u := range a.cfg.Users {
		if u.Username == username {
			user := u
			return &user, nil
		}
	}
	return nil, errors.New("user not found")
}

func (a *App) authenticate(username, password string) bool {
	for _, u := range a.cfg.Users {
		if u.Username == username && u.Password == password {
			return true
		}
	}
	return false
}

func (a *App) mailboxConfig(name string) (*MailboxConfig, error) {
	for _, m := range a.cfg.Mailboxes {
		if m.Name == name {
			box := m
			return &box, nil
		}
	}
	return nil, errors.New("mailbox not found")
}

func (a *App) userAllowsMailbox(username, account string) bool {
	user, err := a.findUser(username)
	if err != nil {
		return false
	}
	for _, m := range user.Mailboxes {
		if m == account {
			return true
		}
	}
	return false
}

func (a *App) currentUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return "", false
	}

	session, ok := a.getSession(cookie.Value)
	if !ok {
		return "", false
	}

	return session.Username, true
}

func (a *App) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := a.currentUser(r)

		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		next.ServeHTTP(w, r)
	}
}

func renderTemplate(w http.ResponseWriter, tpl *template.Template, data any) {
	err := tpl.Execute(w, data)
	if err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (a *App) connectMailbox(name string) (*client.Client, error) {
	cfg, err := a.mailboxConfig(name)
	if err != nil {
		return nil, err
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server, cfg.Port)

	c, err := client.DialTLS(addr, &tls.Config{
		ServerName: cfg.Server,
	})
	if err != nil {
		return nil, err
	}

	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		c.Logout()
		return nil, err
	}

	return c, nil
}

func (a *App) listMailboxes(account string) ([]Folder, error) {
	c, err := a.connectMailbox(account)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	ch := make(chan *imap.MailboxInfo, 50)
	done := make(chan error, 1)

	go func() {
		done <- c.List("", "*", ch)
	}()

	var folders []Folder

	for m := range ch {
		if m == nil {
			continue
		}
		folders = append(folders, Folder{
			Name: m.Name,
		})
	}

	if err := <-done; err != nil {
		return nil, err
	}

	sort.Slice(folders, func(i, j int) bool {
		return folders[i].Name < folders[j].Name
	})

	return folders, nil
}

func (a *App) listMessages(account, folder string) ([]MailSummary, error) {
	c, err := a.connectMailbox(account)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	_, err = c.Select(folder, false)
	if err != nil {
		return nil, err
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{"\\Deleted"}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, err
	}

	if len(uids) == 0 {
		return []MailSummary{}, nil
	}

	start := 0
	if len(uids) > 100 {
		start = len(uids) - 100
	}

	uids = uids[start:]

	uidset := new(imap.SeqSet)
	uidset.AddNum(uids...)

	items := []imap.FetchItem{
		imap.FetchEnvelope,
	}

	ch := make(chan *imap.Message, len(uids))

	err = c.UidFetch(uidset, items, ch)
	if err != nil {
		return nil, err
	}

	var result []MailSummary

	for msg := range ch {
		if msg == nil || msg.Envelope == nil {
			continue
		}

		from := ""

		if len(msg.Envelope.From) > 0 {
			addr := msg.Envelope.From[0]

			if addr.PersonalName != "" {
				from = addr.PersonalName
			} else {
				from = addr.MailboxName + "@" + addr.HostName
			}
		}

		result = append(result, MailSummary{
			UID:     msg.Uid,
			Subject: msg.Envelope.Subject,
			From:    from,
			Date:    msg.Envelope.Date.Format(time.RFC3339),
		})
	}

	slices.Reverse(result)

	return result, nil
}

func (a *App) fetchRawMessage(account, folder string, uid uint32) ([]byte, error) {
	c, err := a.connectMailbox(account)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	_, err = c.Select(folder, false)
	if err != nil {
		return nil, err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	section := &imap.BodySectionName{}

	items := []imap.FetchItem{
		imap.FetchUid,
		section.FetchItem(),
	}

	ch := make(chan *imap.Message, 1)

	err = c.UidFetch(seqset, items, ch)
	if err != nil {
		return nil, err
	}

	msg := <-ch

	if msg == nil {
		return nil, fmt.Errorf("message not found")
	}

	body := msg.GetBody(section)
	if body == nil {
		return nil, fmt.Errorf("message body missing")
	}

	return io.ReadAll(body)
}

func parseMessage(raw []byte) (*ParsedMessage, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	header := mr.Header

	subject, _ := header.Subject()

	var from string

	addrHeader, err := header.AddressList("From")
	if err == nil && len(addrHeader) > 0 {
		from = addrHeader[0].String()
	}

	dateHeader, _ := header.Date()

	var dateStr string
	if !dateHeader.IsZero() {
		dateStr = dateHeader.Format(time.RFC3339)
	}

	pm := &ParsedMessage{
		Subject: subject,
		From:    from,
		Date:    dateStr,
		Raw:     string(raw),
	}

	for {
		part, err := mr.NextPart()

		if err == io.EOF {
			break
		}

		if err != nil {
			return pm, nil
		}

		switch h := part.Header.(type) {

		case *mail.InlineHeader:

			contentType, _, _ := h.ContentType()

			body, err := io.ReadAll(part.Body)
			if err != nil {
				continue
			}

			switch contentType {

			case "text/plain":
				if pm.Text == "" {
					pm.Text = string(body)
				}

			case "text/html":
				if pm.HTML == "" {
					pm.HTML = string(body)
				}
			}
		}
	}

	return pm, nil
}

func sanitizeHTML(html string) template.HTML {
	if html == "" {
		return ""
	}

	p := bluemonday.UGCPolicy()

	// extra hardening
	p.AllowElements("b", "i", "u", "p", "br", "div", "pre")
	p.RequireNoReferrerOnLinks(true)

	safe := p.Sanitize(html)

	return template.HTML(safe)
}

func parseUID(value string) (uint32, error) {
	v, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, err
	}

	return uint32(v), nil
}

func (a *App) loginHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {

	case http.MethodGet:
		renderTemplate(w, loginTemplate, map[string]any{})
		return

	case http.MethodPost:

		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")

		if !a.authenticate(username, password) {
			renderTemplate(w, loginTemplate, map[string]any{
				"Error": "Invalid username or password",
			})
			return
		}

		token := a.createSession(username)

		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   86400, // match session lifetime
		})

		http.Redirect(w, r, "/", http.StatusFound)
		return

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("session")
	if err == nil {
		a.deleteSession(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) homeHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := a.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	user, err := a.findUser(username)
	if err != nil {
		http.Error(w, "user not found", 500)
		return
	}

	renderTemplate(w, mailboxesTemplate, map[string]any{
		"Username": username,
		"Accounts": user.Mailboxes,
	})
}

func (a *App) messageHandler(w http.ResponseWriter, r *http.Request) {
	a.handleMessage(w, r, false)
}

func (a *App) rawMessageHandler(w http.ResponseWriter, r *http.Request) {
	a.handleMessage(w, r, true)
}

func (a *App) handleMessage(w http.ResponseWriter, r *http.Request, rawOnly bool) {
	username, ok := a.currentUser(r)
	if !ok {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/mailbox/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}

	account, folder, uidStr := parts[0], parts[1], parts[2]

	if !a.userAllowsMailbox(username, account) {
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	uid, err := parseUID(uidStr)
	if err != nil {
		http.Error(w, "invalid uid", 400)
		return
	}

	raw, err := a.fetchRawMessage(account, folder, uid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if rawOnly {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(raw)
		return
	}

	msg, err := parseMessage(raw)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	renderTemplate(w, messageTemplate, MessageView{
		Account: account,
		Folder:  folder,
		UID:     uid,
		Subject: msg.Subject,
		From:    msg.From,
		Date:    msg.Date,
		Text:    msg.Text,
		HTML:    sanitizeHTML(msg.HTML),
	})
}

func (a *App) mailboxRouter(w http.ResponseWriter, r *http.Request) {
	username, ok := a.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/mailbox/")
	parts := strings.Split(path, "/")

	// handle /mailbox/ edge case
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	account := parts[0]

	if !a.userAllowsMailbox(username, account) {
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	// /mailbox/account  → folders
	if len(parts) == 1 {
		a.mailboxListFoldersHandler(w, r)
		return
	}

	// /mailbox/account/folder → message list
	if len(parts) == 2 {
		folder := parts[1]

		messages, err := a.listMessages(account, folder)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		renderTemplate(w, inboxTemplate, map[string]any{
			"Account":  account,
			"Folder":   folder,
			"Messages": messages,
		})
		return
	}

	// /mailbox/account/folder/uid OR /raw
	if len(parts) >= 3 {

		// raw: /mailbox/account/folder/uid/raw
		if len(parts) == 4 && parts[3] == "raw" {
			a.rawMessageHandler(w, r)
			return
		}

		// normal message: /mailbox/account/folder/uid
		a.messageHandler(w, r)
		return
	}

	http.NotFound(w, r)
}

func (a *App) mailboxListFoldersHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := a.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	account := strings.TrimPrefix(r.URL.Path, "/mailbox/")
	account = strings.Split(account, "/")[0]

	if !a.userAllowsMailbox(username, account) {
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	folders, err := a.listMailboxes(account)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	renderTemplate(w, foldersTemplate, map[string]any{
		"Account": account,
		"Folders": folders,
	})
}

func (a *App) cleanupSessions() {
	now := time.Now()

	// collect expired keys first (short lock)
	a.mu.RLock()
	expired := make([]string, 0)
	for k, v := range a.sessions {
		if now.Sub(v.Created) > 24*time.Hour {
			expired = append(expired, k)
		}
	}
	a.mu.RUnlock()

	if len(expired) == 0 {
		return
	}

	// delete under write lock (short critical section)
	a.mu.Lock()
	for _, k := range expired {
		// double-check in case it was refreshed/replaced
		if v, ok := a.sessions[k]; ok && now.Sub(v.Created) > 24*time.Hour {
			delete(a.sessions, k)
		} else {
			_ = v
		}
	}
	a.mu.Unlock()
}

func main() {
	configPath := "config.yaml"

	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	app := &App{
		cfg:      cfg,
		sessions: map[string]Session{},
	}

	go func() {
		ticker := time.NewTicker(60 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			app.cleanupSessions()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/login", app.loginHandler)
	mux.HandleFunc("/logout", app.logoutHandler)
	mux.HandleFunc("/", app.authMiddleware(app.homeHandler))
	mux.HandleFunc("/mailbox/", app.authMiddleware(app.mailboxRouter))

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("Listening on %s", cfg.Server.Listen)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
