package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	sessionCookieName    = "readperfect_session"
	googleStateCookie    = "readperfect_google_state"
	defaultSessionLength = 24 * time.Hour * 14
)

var (
	errInvalidCredentials = errors.New("invalid credentials")
	errEmailInUse         = errors.New("email already in use")
)

type contextKey string

const userContextKey contextKey = "current_user"

type application struct {
	db                  *sql.DB
	templateCache       map[string]*template.Template
	staticFS            http.Handler
	sessionCookieSecure bool
	googleOAuth         *oauth2.Config
}

type templateData struct {
	CurrentUser        *User
	GoogleLoginEnabled bool
	Flash              string
	Form               map[string]string
	Errors             map[string]string
	Stats              dashboardStats
	Books              []Book
	Book               *Book
	Questions          []AuthorQuestion
	Invitations        []ReviewInvitation
	GeneratedInviteURL string
	Invitation         *ReviewInvitation
	InviteBook         *Book
	NextPath           string
	InviteAccepted     bool
	ReviewBook         *Book
	ReviewerSubmission *FeedbackSubmission
	ReviewEntries      []FeedbackEntry
	SubmittedFeedback  []SubmittedFeedbackGroup
}

type dashboardStats struct {
	BooksOwned         int
	PendingInvitations int
	SubmittedFeedback  int
}

type User struct {
	ID        int64
	Email     string
	Name      string
	IsAdmin   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Book struct {
	ID          int64
	OwnerUserID int64
	Title       string
	AuthorName  string
	ISBN        string
	CoverURL    string
	Description string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type AuthorQuestion struct {
	ID       int64
	BookID   int64
	Question string
	Position int
}

type ReviewInvitation struct {
	ID         int64
	BookID     int64
	Email      string
	Status     string
	ExpiresAt  time.Time
	AcceptedAt sql.NullTime
	AcceptedBy sql.NullInt64
	CreatedAt  time.Time
}

type FeedbackSubmission struct {
	ID             int64
	BookID         int64
	ReviewerUserID int64
	Status         string
	SubmittedAt    sql.NullTime
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type FeedbackEntry struct {
	ID             int64
	SubmissionID   int64
	EntryType      string
	PageNumber     sql.NullInt64
	ChapterLabel   sql.NullString
	AnchorText     sql.NullString
	CommentBody    string
	Tag            sql.NullString
	QuestionID     sql.NullInt64
	Position       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	AuthorReaction sql.NullString
	AuthorComment  sql.NullString
}

type SubmittedFeedbackGroup struct {
	SubmissionID   int64
	ReviewerUserID int64
	ReviewerName   string
	ReviewerEmail  string
	SubmittedAt    time.Time
	Entries        []FeedbackEntry
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	if err := loadDotEnv(".env"); err != nil {
		logger.Fatal(err)
	}

	db, err := openDB()
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()

	if err := runMigrations(db); err != nil {
		logger.Fatal(err)
	}

	staticRoot, err := fsSub("static")
	if err != nil {
		logger.Fatal(err)
	}

	cache, err := newTemplateCache()
	if err != nil {
		logger.Fatal(err)
	}

	app := &application{
		db:                  db,
		templateCache:       cache,
		staticFS:            http.StripPrefix("/static/", http.FileServer(http.FS(staticRoot))),
		sessionCookieSecure: os.Getenv("COOKIE_SECURE") == "true",
		googleOAuth:         newGoogleOAuthConfig(),
	}

	if adminEmail := normalizeEmail(os.Getenv("BOOTSTRAP_ADMIN_EMAIL")); adminEmail != "" {
		if err := app.bootstrapAdmin(adminEmail); err != nil {
			logger.Printf("bootstrap admin failed: %v", err)
		}
	}

	server := &http.Server{
		Addr:         listenAddr(),
		Handler:      app.routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	logger.Printf("readperfect listening on %s", server.Addr)
	logger.Fatal(server.ListenAndServe())
}

func listenAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}

	return ":8080"
}

func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", app.staticFS)
	mux.HandleFunc("/", app.home)
	mux.HandleFunc("/login", app.login)
	mux.HandleFunc("/signup", app.signup)
	mux.HandleFunc("/logout", app.logout)
	mux.HandleFunc("/auth/google/start", app.googleStart)
	mux.HandleFunc("/auth/google/callback", app.googleCallback)
	mux.HandleFunc("/invites/", app.invitesRouter)
	mux.Handle("/entries/", app.requireAuth(http.HandlerFunc(app.entriesRouter)))
	mux.Handle("/app", app.requireAuth(http.HandlerFunc(app.dashboard)))
	mux.Handle("/reviews/", app.requireAuth(http.HandlerFunc(app.reviewsRouter)))
	mux.Handle("/books/new", app.requireAuth(http.HandlerFunc(app.newBook)))
	mux.Handle("/books", app.requireAuth(http.HandlerFunc(app.createBook)))
	mux.Handle("/books/", app.requireAuth(http.HandlerFunc(app.booksRouter)))

	return app.loadUser(mux)
}

func (app *application) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := app.newTemplateData(r)
	app.render(w, http.StatusOK, "home", data)
}

func (app *application) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if app.currentUser(r) != nil {
			http.Redirect(w, r, "/app", http.StatusSeeOther)
			return
		}

		data := app.newTemplateData(r)
		data.Form = map[string]string{}
		data.NextPath = safeRedirectPath(r.URL.Query().Get("next"))
		app.render(w, http.StatusOK, "login", data)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	nextPath := safeRedirectPath(r.FormValue("next"))

	data := app.newTemplateData(r)
	data.Form = map[string]string{"email": email, "next": nextPath}
	data.Errors = make(map[string]string)
	data.NextPath = nextPath

	if email == "" {
		data.Errors["email"] = "Enter your email."
	}
	if password == "" {
		data.Errors["password"] = "Enter your password."
	}
	if len(data.Errors) > 0 {
		app.render(w, http.StatusUnprocessableEntity, "login", data)
		return
	}

	user, err := app.authenticateUser(email, password)
	if err != nil {
		data.Errors["generic"] = "We could not sign you in with those details."
		app.render(w, http.StatusUnauthorized, "login", data)
		return
	}

	if err := app.startSession(w, user.ID); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, app.afterAuthRedirect(nextPath), http.StatusSeeOther)
}

func (app *application) signup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if app.currentUser(r) != nil {
			http.Redirect(w, r, "/app", http.StatusSeeOther)
			return
		}

		data := app.newTemplateData(r)
		data.Form = map[string]string{}
		data.NextPath = safeRedirectPath(r.URL.Query().Get("next"))
		app.render(w, http.StatusOK, "signup", data)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	nextPath := safeRedirectPath(r.FormValue("next"))

	data := app.newTemplateData(r)
	data.Form = map[string]string{
		"name":  name,
		"email": email,
		"next":  nextPath,
	}
	data.Errors = make(map[string]string)
	data.NextPath = nextPath

	switch {
	case name == "":
		data.Errors["name"] = "Enter your name."
	case len(name) > 120:
		data.Errors["name"] = "Name is too long."
	}
	if email == "" {
		data.Errors["email"] = "Enter your email."
	}
	if len(password) < 8 {
		data.Errors["password"] = "Use at least 8 characters."
	}

	if len(data.Errors) > 0 {
		app.render(w, http.StatusUnprocessableEntity, "signup", data)
		return
	}

	user, err := app.createUserWithPassword(name, email, password)
	if err != nil {
		if errors.Is(err, errEmailInUse) {
			data.Errors["email"] = "An account with that email already exists."
			app.render(w, http.StatusConflict, "signup", data)
			return
		}
		http.Error(w, "could not create user", http.StatusInternalServerError)
		return
	}

	if err := app.startSession(w, user.ID); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, app.afterAuthRedirect(nextPath), http.StatusSeeOther)
}

func (app *application) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		_ = app.deleteSession(cookie.Value)
	}

	app.expireCookie(w, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) dashboard(w http.ResponseWriter, r *http.Request) {
	user := app.currentUser(r)
	stats, err := app.dashboardStats(user.ID)
	if err != nil {
		http.Error(w, "could not load dashboard", http.StatusInternalServerError)
		return
	}
	books, err := app.listBooksByOwner(user.ID)
	if err != nil {
		http.Error(w, "could not load books", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Stats = stats
	data.Books = books
	app.render(w, http.StatusOK, "dashboard", data)
}

func (app *application) newBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := app.newTemplateData(r)
	data.Form = map[string]string{}
	app.render(w, http.StatusOK, "book_new", data)
}

func (app *application) createBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	authorName := strings.TrimSpace(r.FormValue("author_name"))
	isbn := strings.TrimSpace(r.FormValue("isbn"))
	description := strings.TrimSpace(r.FormValue("description"))

	data := app.newTemplateData(r)
	data.Form = map[string]string{
		"title":       title,
		"author_name": authorName,
		"isbn":        isbn,
		"description": description,
	}
	data.Errors = make(map[string]string)

	if title == "" {
		data.Errors["title"] = "Enter the book title."
	}
	if authorName == "" {
		data.Errors["author_name"] = "Enter the author name."
	}
	if len(title) > 200 {
		data.Errors["title"] = "Title is too long."
	}
	if len(authorName) > 200 {
		data.Errors["author_name"] = "Author name is too long."
	}
	if len(isbn) > 64 {
		data.Errors["isbn"] = "ISBN is too long."
	}
	if len(description) > 4000 {
		data.Errors["description"] = "Description is too long."
	}

	if len(data.Errors) > 0 {
		app.render(w, http.StatusUnprocessableEntity, "book_new", data)
		return
	}

	user := app.currentUser(r)
	if _, err := app.insertBook(user.ID, title, authorName, isbn, description); err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/app?flash=Book+created.", http.StatusSeeOther)
}

func (app *application) booksRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/books/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	bookID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.showBook(w, r, bookID)
	case len(parts) == 2 && parts[1] == "questions" && r.Method == http.MethodPost:
		app.createQuestion(w, r, bookID)
	case len(parts) == 2 && parts[1] == "invitations" && r.Method == http.MethodPost:
		app.createInvitation(w, r, bookID)
	default:
		http.NotFound(w, r)
	}
}

func (app *application) invitesRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/invites/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	token := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.showInvite(w, r, token)
	case len(parts) == 2 && parts[1] == "accept" && r.Method == http.MethodPost:
		app.acceptInvite(w, r, token)
	default:
		http.NotFound(w, r)
	}
}

func (app *application) reviewsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/reviews/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	bookID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.showReviewerWorkspace(w, r, bookID)
	case len(parts) == 2 && parts[1] == "entries" && r.Method == http.MethodPost:
		app.createReviewEntry(w, r, bookID)
	case len(parts) == 2 && parts[1] == "submit" && r.Method == http.MethodPost:
		app.submitReviewerDraft(w, r, bookID)
	default:
		http.NotFound(w, r)
	}
}

func (app *application) entriesRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/entries/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "respond" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	entryID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	app.respondToEntry(w, r, entryID)
}

func (app *application) showBook(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	book, err := app.getBookForOwner(bookID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load book", http.StatusInternalServerError)
		return
	}

	questions, err := app.listQuestionsForBook(bookID)
	if err != nil {
		http.Error(w, "could not load questions", http.StatusInternalServerError)
		return
	}

	invitations, err := app.listInvitationsForBook(bookID)
	if err != nil {
		http.Error(w, "could not load invitations", http.StatusInternalServerError)
		return
	}
	submittedFeedback, err := app.listSubmittedFeedbackForBook(bookID)
	if err != nil {
		log.Printf("showBook submitted feedback book=%d err=%v", bookID, err)
		http.Error(w, "could not load submitted feedback", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Book = book
	data.Questions = questions
	data.Invitations = invitations
	data.SubmittedFeedback = submittedFeedback
	app.render(w, http.StatusOK, "book_show", data)
}

func (app *application) createQuestion(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	book, err := app.getBookForOwner(bookID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load book", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" {
		app.renderBookWorkspace(w, r, book, "book_show", map[string]string{"question": "Enter a question for reviewers."}, "", http.StatusUnprocessableEntity)
		return
	}
	if len(question) > 500 {
		app.renderBookWorkspace(w, r, book, "book_show", map[string]string{"question": "Question is too long."}, "", http.StatusUnprocessableEntity)
		return
	}

	if err := app.insertQuestion(bookID, question); err != nil {
		http.Error(w, "could not save question", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Question+added.", bookID), http.StatusSeeOther)
}

func (app *application) createInvitation(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	book, err := app.getBookForOwner(bookID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load book", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	email := normalizeEmail(r.FormValue("email"))
	if email == "" {
		app.renderBookWorkspace(w, r, book, "book_show", map[string]string{"invite_email": "Enter the reviewer email."}, "", http.StatusUnprocessableEntity)
		return
	}

	inviteToken, err := randomToken(32)
	if err != nil {
		http.Error(w, "could not create invite", http.StatusInternalServerError)
		return
	}

	if err := app.insertInvitation(bookID, user.ID, email, inviteToken); err != nil {
		http.Error(w, "could not create invite", http.StatusInternalServerError)
		return
	}

	inviteURL := app.absoluteURL(r, "/invites/"+inviteToken)
	app.renderBookWorkspace(w, r, book, "book_show", nil, inviteURL, http.StatusOK)
}

func (app *application) renderBookWorkspace(w http.ResponseWriter, r *http.Request, book *Book, page string, errorsMap map[string]string, inviteURL string, status int) {
	questions, err := app.listQuestionsForBook(book.ID)
	if err != nil {
		http.Error(w, "could not load questions", http.StatusInternalServerError)
		return
	}

	invitations, err := app.listInvitationsForBook(book.ID)
	if err != nil {
		http.Error(w, "could not load invitations", http.StatusInternalServerError)
		return
	}
	submittedFeedback, err := app.listSubmittedFeedbackForBook(book.ID)
	if err != nil {
		http.Error(w, "could not load submitted feedback", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Book = book
	data.Questions = questions
	data.Invitations = invitations
	data.SubmittedFeedback = submittedFeedback
	data.GeneratedInviteURL = inviteURL
	if errorsMap != nil {
		data.Errors = errorsMap
	}
	app.render(w, status, page, data)
}

func (app *application) showInvite(w http.ResponseWriter, r *http.Request, rawToken string) {
	invitation, book, err := app.getInvitationByToken(rawToken)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load invitation", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Invitation = invitation
	data.InviteBook = book
	data.NextPath = "/invites/" + rawToken

	if invitation.Status != "pending" {
		data.Errors["invite"] = "This invite is no longer available."
		app.render(w, http.StatusOK, "invite_show", data)
		return
	}

	if time.Now().UTC().After(invitation.ExpiresAt) {
		data.Errors["invite"] = "This invite has expired."
		app.render(w, http.StatusOK, "invite_show", data)
		return
	}

	app.render(w, http.StatusOK, "invite_show", data)
}

func (app *application) acceptInvite(w http.ResponseWriter, r *http.Request, rawToken string) {
	user := app.currentUser(r)
	if user == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/invites/"+rawToken), http.StatusSeeOther)
		return
	}

	invitation, book, err := app.getInvitationByToken(rawToken)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load invitation", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Invitation = invitation
	data.InviteBook = book
	data.NextPath = "/invites/" + rawToken

	switch {
	case invitation.Status != "pending":
		data.Errors["invite"] = "This invite is no longer available."
		app.render(w, http.StatusOK, "invite_show", data)
		return
	case time.Now().UTC().After(invitation.ExpiresAt):
		data.Errors["invite"] = "This invite has expired."
		app.render(w, http.StatusOK, "invite_show", data)
		return
	case normalizeEmail(user.Email) != normalizeEmail(invitation.Email):
		data.Errors["invite"] = "This invite was sent to a different email address. Sign in with the invited email to accept it."
		app.render(w, http.StatusForbidden, "invite_show", data)
		return
	}

	if err := app.acceptInvitation(invitation.ID, user.ID); err != nil {
		http.Error(w, "could not accept invite", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/reviews/%d?flash=Invite+accepted.", invitation.BookID), http.StatusSeeOther)
}

func (app *application) showReviewerWorkspace(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	book, err := app.getBookForReviewer(bookID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	submission, entries, err := app.ensureDraftSubmission(bookID, user.ID)
	if err != nil {
		http.Error(w, "could not load draft submission", http.StatusInternalServerError)
		return
	}
	submittedFeedback, err := app.listSubmittedFeedbackForReviewer(bookID, user.ID)
	if err != nil {
		log.Printf("showReviewerWorkspace submitted feedback book=%d reviewer=%d err=%v", bookID, user.ID, err)
		http.Error(w, "could not load submitted feedback", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.ReviewBook = book
	data.ReviewerSubmission = submission
	data.ReviewEntries = entries
	data.SubmittedFeedback = submittedFeedback
	app.render(w, http.StatusOK, "review_show", data)
}

func (app *application) createReviewEntry(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	book, err := app.getBookForReviewer(bookID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	submission, entries, err := app.ensureDraftSubmission(bookID, user.ID)
	if err != nil {
		log.Printf("showReviewerWorkspace draft submission book=%d reviewer=%d err=%v", bookID, user.ID, err)
		http.Error(w, "could not load draft submission", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	entryType := strings.TrimSpace(r.FormValue("entry_type"))
	pageValue := strings.TrimSpace(r.FormValue("page_number"))
	chapterLabel := strings.TrimSpace(r.FormValue("chapter_label"))
	anchorText := strings.TrimSpace(r.FormValue("anchor_text"))
	commentBody := strings.TrimSpace(r.FormValue("comment_body"))

	data := app.newTemplateData(r)
	data.ReviewBook = book
	data.ReviewerSubmission = submission
	data.ReviewEntries = entries
	data.SubmittedFeedback, _ = app.listSubmittedFeedbackForReviewer(bookID, user.ID)
	data.Form = map[string]string{
		"entry_type":    entryType,
		"page_number":   pageValue,
		"chapter_label": chapterLabel,
		"anchor_text":   anchorText,
		"comment_body":  commentBody,
	}
	data.Errors = make(map[string]string)

	if entryType != "page" && entryType != "chapter" {
		data.Errors["entry_type"] = "Choose page note or chapter note."
	}
	if commentBody == "" {
		data.Errors["comment_body"] = "Enter your note."
	}

	var pageNumber *int
	switch entryType {
	case "page":
		if pageValue == "" {
			data.Errors["page_number"] = "Enter the page number."
		} else {
			n, err := strconv.Atoi(pageValue)
			if err != nil || n <= 0 {
				data.Errors["page_number"] = "Enter a valid page number."
			} else {
				pageNumber = &n
			}
		}
	case "chapter":
		if chapterLabel == "" {
			data.Errors["chapter_label"] = "Enter the chapter label."
		}
	}

	if len(data.Errors) > 0 {
		app.render(w, http.StatusUnprocessableEntity, "review_show", data)
		return
	}

	if err := app.insertFeedbackEntry(submission.ID, entryType, pageNumber, chapterLabel, anchorText, commentBody); err != nil {
		http.Error(w, "could not save note", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/reviews/%d?flash=Note+saved.", bookID), http.StatusSeeOther)
}

func (app *application) submitReviewerDraft(w http.ResponseWriter, r *http.Request, bookID int64) {
	user := app.currentUser(r)
	if _, err := app.getBookForReviewer(bookID, user.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	submission, entries, err := app.ensureDraftSubmission(bookID, user.ID)
	if err != nil {
		http.Error(w, "could not load draft submission", http.StatusInternalServerError)
		return
	}
	if len(entries) == 0 {
		http.Redirect(w, r, fmt.Sprintf("/reviews/%d?flash=Add+at+least+one+note+before+submitting.", bookID), http.StatusSeeOther)
		return
	}

	if err := app.submitFeedbackSubmission(submission.ID); err != nil {
		http.Error(w, "could not submit feedback", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/reviews/%d?flash=Feedback+submitted.", bookID), http.StatusSeeOther)
}

func (app *application) respondToEntry(w http.ResponseWriter, r *http.Request, entryID int64) {
	user := app.currentUser(r)
	entry, bookID, err := app.getSubmittedEntryForAuthor(entryID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load feedback entry", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	action := strings.TrimSpace(r.FormValue("action"))
	reaction := strings.TrimSpace(r.FormValue("reaction"))
	comment := strings.TrimSpace(r.FormValue("comment"))

	if reaction != "" && reaction != "insightful" {
		http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Unsupported+reaction.", bookID), http.StatusSeeOther)
		return
	}
	if len(comment) > 2000 {
		http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Comment+is+too+long.", bookID), http.StatusSeeOther)
		return
	}

	switch action {
	case "toggle_reaction":
		if reaction == "" {
			http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Unsupported+reaction.", bookID), http.StatusSeeOther)
			return
		}
		if entry.AuthorReaction.Valid && entry.AuthorReaction.String == reaction {
			reaction = ""
		}
		comment = currentNullString(entry.AuthorComment)
	case "save_comment":
		reaction = currentNullString(entry.AuthorReaction)
	default:
		http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Unsupported+response+action.", bookID), http.StatusSeeOther)
		return
	}

	if err := app.upsertEntryResponse(entry.ID, reaction, comment); err != nil {
		http.Error(w, "could not save response", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/books/%d?flash=Response+saved.", bookID), http.StatusSeeOther)
}

func (app *application) googleStart(w http.ResponseWriter, r *http.Request) {
	if app.googleOAuth == nil {
		http.Redirect(w, r, "/login?flash=Google+sign-in+is+not+configured+yet.", http.StatusSeeOther)
		return
	}

	state, err := randomToken(32)
	if err != nil {
		http.Error(w, "could not start google auth", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     googleStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   app.sessionCookieSecure,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})

	url := app.googleOAuth.AuthCodeURL(state, oauth2.AccessTypeOnline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (app *application) googleCallback(w http.ResponseWriter, r *http.Request) {
	if app.googleOAuth == nil {
		http.Redirect(w, r, "/login?flash=Google+sign-in+is+not+configured+yet.", http.StatusSeeOther)
		return
	}

	stateCookie, err := r.Cookie(googleStateCookie)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/login?flash=Google+sign-in+could+not+be+verified.", http.StatusSeeOther)
		return
	}
	app.expireCookie(w, googleStateCookie)

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/login?flash=Google+did+not+return+a+valid+login.", http.StatusSeeOther)
		return
	}

	token, err := app.googleOAuth.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/login?flash=Google+sign-in+failed.", http.StatusSeeOther)
		return
	}

	profile, err := fetchGoogleProfile(r.Context(), app.googleOAuth, token)
	if err != nil {
		http.Redirect(w, r, "/login?flash=Google+account+details+could+not+be+read.", http.StatusSeeOther)
		return
	}

	user, err := app.upsertGoogleUser(profile)
	if err != nil {
		http.Error(w, "could not complete google sign-in", http.StatusInternalServerError)
		return
	}

	if err := app.startSession(w, user.ID); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (app *application) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.currentUser(r) == nil {
			http.Redirect(w, r, "/login?flash=Sign+in+to+continue.", http.StatusSeeOther)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (app *application) loadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, err := app.getUserBySessionToken(cookie.Value)
		if err != nil {
			app.expireCookie(w, sessionCookieName)
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (app *application) currentUser(r *http.Request) *User {
	user, ok := r.Context().Value(userContextKey).(*User)
	if !ok {
		return nil
	}

	return user
}

func (app *application) newTemplateData(r *http.Request) *templateData {
	return &templateData{
		CurrentUser:        app.currentUser(r),
		GoogleLoginEnabled: app.googleOAuth != nil,
		Flash:              r.URL.Query().Get("flash"),
		Form:               map[string]string{},
		Errors:             map[string]string{},
	}
}

func (app *application) render(w http.ResponseWriter, status int, page string, data *templateData) {
	tmpl, ok := app.templateCache[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func newTemplateCache() (map[string]*template.Template, error) {
	pages := []string{"home", "login", "signup", "dashboard", "book_new", "book_show", "invite_show", "review_show"}
	cache := make(map[string]*template.Template, len(pages))

	for _, page := range pages {
		files := []string{
			"templates/base.html",
			fmt.Sprintf("templates/%s.html", page),
		}

		tmpl, err := template.ParseFS(assets, files...)
		if err != nil {
			return nil, err
		}

		cache[page] = tmpl
	}

	return cache, nil
}

func openDB() (*sql.DB, error) {
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = filepath.Join(".", "readperfect.db")
	}

	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(3 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, err
	}

	return db, nil
}

func runMigrations(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS password_credentials (
			user_id INTEGER PRIMARY KEY,
			password_hash TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS oauth_identities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			provider_user_id TEXT NOT NULL,
			email_at_provider TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			UNIQUE(provider, provider_user_id),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);`,
		`CREATE TABLE IF NOT EXISTS books (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			owner_user_id INTEGER NOT NULL,
			title TEXT NOT NULL,
			author_name TEXT NOT NULL,
			isbn TEXT,
			cover_url TEXT,
			description TEXT,
			status TEXT NOT NULL DEFAULT 'draft',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS author_questions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id INTEGER NOT NULL,
			question TEXT NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS review_invitations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id INTEGER NOT NULL,
			email TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'pending',
			invited_by_user_id INTEGER NOT NULL,
			expires_at DATETIME NOT NULL,
			accepted_at DATETIME,
			accepted_by_user_id INTEGER,
			created_at DATETIME NOT NULL,
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE,
			FOREIGN KEY (invited_by_user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (accepted_by_user_id) REFERENCES users(id) ON DELETE SET NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_review_invitations_book_id ON review_invitations(book_id);`,
		`CREATE TABLE IF NOT EXISTS book_reviewers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			invitation_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			UNIQUE(book_id, user_id),
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (invitation_id) REFERENCES review_invitations(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS feedback_submissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id INTEGER NOT NULL,
			reviewer_user_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'draft',
			submitted_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE,
			FOREIGN KEY (reviewer_user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS feedback_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			submission_id INTEGER NOT NULL,
			entry_type TEXT NOT NULL,
			page_number INTEGER,
			chapter_label TEXT,
			anchor_text TEXT,
			comment_body TEXT NOT NULL,
			tag TEXT,
			question_id INTEGER,
			position INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (submission_id) REFERENCES feedback_submissions(id) ON DELETE CASCADE,
			FOREIGN KEY (question_id) REFERENCES author_questions(id) ON DELETE SET NULL
		);`,
		`CREATE TABLE IF NOT EXISTS feedback_entry_responses (
			feedback_entry_id INTEGER PRIMARY KEY,
			author_reaction TEXT,
			author_comment TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (feedback_entry_id) REFERENCES feedback_entries(id) ON DELETE CASCADE
		);`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
}

func (app *application) createUserWithPassword(name, email, password string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	tx, err := app.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO users (email, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, email, name, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, errEmailInUse
		}
		return nil, err
	}

	userID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`INSERT INTO password_credentials (user_id, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`, userID, string(hash), now, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &User{
		ID:        userID,
		Email:     email,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (app *application) authenticateUser(email, password string) (*User, error) {
	var user User
	var passwordHash string
	err := app.db.QueryRow(`
		SELECT u.id, u.email, u.name, u.is_admin, u.created_at, u.updated_at, pc.password_hash
		FROM users u
		JOIN password_credentials pc ON pc.user_id = u.id
		WHERE u.email = ?`, email).Scan(
		&user.ID,
		&user.Email,
		&user.Name,
		&user.IsAdmin,
		&user.CreatedAt,
		&user.UpdatedAt,
		&passwordHash,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errInvalidCredentials
		}
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return nil, errInvalidCredentials
	}

	return &user, nil
}

type googleProfile struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
}

func (app *application) upsertGoogleUser(profile googleProfile) (*User, error) {
	email := normalizeEmail(profile.Email)
	if !profile.VerifiedEmail || email == "" || profile.ID == "" {
		return nil, errInvalidCredentials
	}

	now := time.Now().UTC()
	tx, err := app.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var user User
	err = tx.QueryRow(`SELECT id, email, name, is_admin, created_at, updated_at FROM users WHERE email = ?`, email).Scan(
		&user.ID,
		&user.Email,
		&user.Name,
		&user.IsAdmin,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	switch {
	case err == nil:
		if user.Name == "" && profile.Name != "" {
			if _, err := tx.Exec(`UPDATE users SET name = ?, updated_at = ? WHERE id = ?`, profile.Name, now, user.ID); err != nil {
				return nil, err
			}
			user.Name = profile.Name
			user.UpdatedAt = now
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.Exec(`INSERT INTO users (email, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, email, fallbackName(profile.Name, email), now, now)
		if err != nil {
			return nil, err
		}
		user.ID, err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
		user.Email = email
		user.Name = fallbackName(profile.Name, email)
		user.CreatedAt = now
		user.UpdatedAt = now
	default:
		return nil, err
	}

	_, err = tx.Exec(`
		INSERT INTO oauth_identities (user_id, provider, provider_user_id, email_at_provider, created_at)
		VALUES (?, 'google', ?, ?, ?)
		ON CONFLICT(provider, provider_user_id) DO UPDATE SET
			user_id = excluded.user_id,
			email_at_provider = excluded.email_at_provider
	`, user.ID, profile.ID, email, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &user, nil
}

func (app *application) startSession(w http.ResponseWriter, userID int64) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}

	if err := app.storeSession(userID, token, time.Now().UTC().Add(defaultSessionLength)); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   app.sessionCookieSecure,
		MaxAge:   int(defaultSessionLength.Seconds()),
	})

	return nil
}

func (app *application) storeSession(userID int64, token string, expiresAt time.Time) error {
	_, err := app.db.Exec(`INSERT INTO sessions (user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		userID,
		hashToken(token),
		expiresAt,
		time.Now().UTC(),
	)
	return err
}

func (app *application) deleteSession(token string) error {
	_, err := app.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func (app *application) getUserBySessionToken(token string) (*User, error) {
	now := time.Now().UTC()
	var user User
	err := app.db.QueryRow(`
		SELECT u.id, u.email, u.name, u.is_admin, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?
	`, hashToken(token), now).Scan(
		&user.ID,
		&user.Email,
		&user.Name,
		&user.IsAdmin,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (app *application) dashboardStats(userID int64) (dashboardStats, error) {
	stats := dashboardStats{}

	if err := app.db.QueryRow(`SELECT COUNT(*) FROM books WHERE owner_user_id = ?`, userID).Scan(&stats.BooksOwned); err != nil {
		return stats, err
	}
	if err := app.db.QueryRow(`
		SELECT COUNT(*)
		FROM review_invitations ri
		JOIN books b ON b.id = ri.book_id
		WHERE b.owner_user_id = ? AND ri.status = 'pending'
	`, userID).Scan(&stats.PendingInvitations); err != nil {
		return stats, err
	}
	if err := app.db.QueryRow(`
		SELECT COUNT(*)
		FROM feedback_submissions fs
		JOIN books b ON b.id = fs.book_id
		WHERE b.owner_user_id = ? AND fs.status = 'submitted'
	`, userID).Scan(&stats.SubmittedFeedback); err != nil {
		return stats, err
	}

	return stats, nil
}

func (app *application) listBooksByOwner(userID int64) ([]Book, error) {
	rows, err := app.db.Query(`
		SELECT id, owner_user_id, title, author_name, isbn, cover_url, description, status, created_at, updated_at
		FROM books
		WHERE owner_user_id = ?
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var book Book
		var isbn, coverURL, description sql.NullString
		if err := rows.Scan(
			&book.ID,
			&book.OwnerUserID,
			&book.Title,
			&book.AuthorName,
			&isbn,
			&coverURL,
			&description,
			&book.Status,
			&book.CreatedAt,
			&book.UpdatedAt,
		); err != nil {
			return nil, err
		}
		book.ISBN = isbn.String
		book.CoverURL = coverURL.String
		book.Description = description.String
		books = append(books, book)
	}

	return books, rows.Err()
}

func (app *application) getBookForOwner(bookID, ownerUserID int64) (*Book, error) {
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT id, owner_user_id, title, author_name, isbn, cover_url, description, status, created_at, updated_at
		FROM books
		WHERE id = ? AND owner_user_id = ?
	`, bookID, ownerUserID).Scan(
		&book.ID,
		&book.OwnerUserID,
		&book.Title,
		&book.AuthorName,
		&isbn,
		&coverURL,
		&description,
		&book.Status,
		&book.CreatedAt,
		&book.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	book.ISBN = isbn.String
	book.CoverURL = coverURL.String
	book.Description = description.String
	return &book, nil
}

func (app *application) insertBook(ownerUserID int64, title, authorName, isbn, description string) (*Book, error) {
	now := time.Now().UTC()
	res, err := app.db.Exec(`
		INSERT INTO books (owner_user_id, title, author_name, isbn, description, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'draft', ?, ?)
	`, ownerUserID, title, authorName, nullIfEmpty(isbn), nullIfEmpty(description), now, now)
	if err != nil {
		return nil, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Book{
		ID:          id,
		OwnerUserID: ownerUserID,
		Title:       title,
		AuthorName:  authorName,
		ISBN:        isbn,
		Description: description,
		Status:      "draft",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (app *application) listQuestionsForBook(bookID int64) ([]AuthorQuestion, error) {
	rows, err := app.db.Query(`
		SELECT id, book_id, question, position
		FROM author_questions
		WHERE book_id = ?
		ORDER BY position ASC, id ASC
	`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var questions []AuthorQuestion
	for rows.Next() {
		var question AuthorQuestion
		if err := rows.Scan(&question.ID, &question.BookID, &question.Question, &question.Position); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}

	return questions, rows.Err()
}

func (app *application) insertQuestion(bookID int64, question string) error {
	var nextPosition int
	if err := app.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM author_questions WHERE book_id = ?`, bookID).Scan(&nextPosition); err != nil {
		return err
	}

	_, err := app.db.Exec(`
		INSERT INTO author_questions (book_id, question, position)
		VALUES (?, ?, ?)
	`, bookID, question, nextPosition)
	return err
}

func (app *application) listInvitationsForBook(bookID int64) ([]ReviewInvitation, error) {
	rows, err := app.db.Query(`
		SELECT id, book_id, email, status, expires_at, accepted_at, accepted_by_user_id, created_at
		FROM review_invitations
		WHERE book_id = ?
		ORDER BY created_at DESC
	`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invitations []ReviewInvitation
	for rows.Next() {
		var invitation ReviewInvitation
		if err := rows.Scan(
			&invitation.ID,
			&invitation.BookID,
			&invitation.Email,
			&invitation.Status,
			&invitation.ExpiresAt,
			&invitation.AcceptedAt,
			&invitation.AcceptedBy,
			&invitation.CreatedAt,
		); err != nil {
			return nil, err
		}
		invitations = append(invitations, invitation)
	}

	return invitations, rows.Err()
}

func (app *application) getInvitationByToken(rawToken string) (*ReviewInvitation, *Book, error) {
	var invitation ReviewInvitation
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT
			ri.id, ri.book_id, ri.email, ri.status, ri.expires_at, ri.accepted_at, ri.accepted_by_user_id, ri.created_at,
			b.id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM review_invitations ri
		JOIN books b ON b.id = ri.book_id
		WHERE ri.token_hash = ?
	`, hashToken(rawToken)).Scan(
		&invitation.ID,
		&invitation.BookID,
		&invitation.Email,
		&invitation.Status,
		&invitation.ExpiresAt,
		&invitation.AcceptedAt,
		&invitation.AcceptedBy,
		&invitation.CreatedAt,
		&book.ID,
		&book.OwnerUserID,
		&book.Title,
		&book.AuthorName,
		&isbn,
		&coverURL,
		&description,
		&book.Status,
		&book.CreatedAt,
		&book.UpdatedAt,
	)
	if err != nil {
		return nil, nil, err
	}

	book.ISBN = isbn.String
	book.CoverURL = coverURL.String
	book.Description = description.String

	return &invitation, &book, nil
}

func (app *application) getBookForReviewer(bookID, reviewerUserID int64) (*Book, error) {
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT b.id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM books b
		JOIN book_reviewers br ON br.book_id = b.id
		WHERE b.id = ? AND br.user_id = ?
	`, bookID, reviewerUserID).Scan(
		&book.ID,
		&book.OwnerUserID,
		&book.Title,
		&book.AuthorName,
		&isbn,
		&coverURL,
		&description,
		&book.Status,
		&book.CreatedAt,
		&book.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	book.ISBN = isbn.String
	book.CoverURL = coverURL.String
	book.Description = description.String
	return &book, nil
}

func (app *application) insertInvitation(bookID, invitedByUserID int64, email, rawToken string) error {
	expiresAt := time.Now().UTC().Add(14 * 24 * time.Hour)
	_, err := app.db.Exec(`
		INSERT INTO review_invitations (book_id, email, token_hash, status, invited_by_user_id, expires_at, created_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)
	`, bookID, email, hashToken(rawToken), invitedByUserID, expiresAt, time.Now().UTC())
	return err
}

func (app *application) acceptInvitation(invitationID, userID int64) error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var bookID int64
	if err := tx.QueryRow(`SELECT book_id FROM review_invitations WHERE id = ?`, invitationID).Scan(&bookID); err != nil {
		return err
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		UPDATE review_invitations
		SET status = 'accepted', accepted_at = ?, accepted_by_user_id = ?
		WHERE id = ?
	`, now, userID, invitationID); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO book_reviewers (book_id, user_id, invitation_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(book_id, user_id) DO NOTHING
	`, bookID, userID, invitationID, now); err != nil {
		return err
	}

	return tx.Commit()
}

func (app *application) ensureDraftSubmission(bookID, reviewerUserID int64) (*FeedbackSubmission, []FeedbackEntry, error) {
	var submission FeedbackSubmission
	err := app.db.QueryRow(`
		SELECT id, book_id, reviewer_user_id, status, submitted_at, created_at, updated_at
		FROM feedback_submissions
		WHERE book_id = ? AND reviewer_user_id = ? AND status = 'draft'
		ORDER BY id DESC
		LIMIT 1
	`, bookID, reviewerUserID).Scan(
		&submission.ID,
		&submission.BookID,
		&submission.ReviewerUserID,
		&submission.Status,
		&submission.SubmittedAt,
		&submission.CreatedAt,
		&submission.UpdatedAt,
	)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		now := time.Now().UTC()
		res, err := app.db.Exec(`
			INSERT INTO feedback_submissions (book_id, reviewer_user_id, status, created_at, updated_at)
			VALUES (?, ?, 'draft', ?, ?)
		`, bookID, reviewerUserID, now, now)
		if err != nil {
			return nil, nil, err
		}
		submissionID, err := res.LastInsertId()
		if err != nil {
			return nil, nil, err
		}
		submission = FeedbackSubmission{
			ID:             submissionID,
			BookID:         bookID,
			ReviewerUserID: reviewerUserID,
			Status:         "draft",
			CreatedAt:      now,
			UpdatedAt:      now,
		}
	default:
		return nil, nil, err
	}

	entries, err := app.listFeedbackEntries(submission.ID)
	if err != nil {
		return nil, nil, err
	}
	return &submission, entries, nil
}

func (app *application) listFeedbackEntries(submissionID int64) ([]FeedbackEntry, error) {
	rows, err := app.db.Query(`
		SELECT
			fe.id, fe.submission_id, fe.entry_type, fe.page_number, fe.chapter_label, fe.anchor_text,
			fe.comment_body, fe.tag, fe.question_id, fe.position, fe.created_at, fe.updated_at,
			fer.author_reaction, fer.author_comment
		FROM feedback_entries fe
		LEFT JOIN feedback_entry_responses fer ON fer.feedback_entry_id = fe.id
		WHERE submission_id = ?
		ORDER BY fe.position ASC, fe.id ASC
	`, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []FeedbackEntry
	for rows.Next() {
		var entry FeedbackEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.SubmissionID,
			&entry.EntryType,
			&entry.PageNumber,
			&entry.ChapterLabel,
			&entry.AnchorText,
			&entry.CommentBody,
			&entry.Tag,
			&entry.QuestionID,
			&entry.Position,
			&entry.CreatedAt,
			&entry.UpdatedAt,
			&entry.AuthorReaction,
			&entry.AuthorComment,
		); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (app *application) insertFeedbackEntry(submissionID int64, entryType string, pageNumber *int, chapterLabel, anchorText, commentBody string) error {
	var nextPosition int
	if err := app.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM feedback_entries WHERE submission_id = ?`, submissionID).Scan(&nextPosition); err != nil {
		return err
	}

	now := time.Now().UTC()
	var pageValue any
	if pageNumber != nil {
		pageValue = *pageNumber
	}
	_, err := app.db.Exec(`
		INSERT INTO feedback_entries (submission_id, entry_type, page_number, chapter_label, anchor_text, comment_body, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, submissionID, entryType, pageValue, nullIfEmpty(chapterLabel), nullIfEmpty(anchorText), commentBody, nextPosition, now, now)
	return err
}

func (app *application) getSubmittedEntryForAuthor(entryID, authorUserID int64) (*FeedbackEntry, int64, error) {
	var entry FeedbackEntry
	var bookID int64
	err := app.db.QueryRow(`
		SELECT
			fe.id, fe.submission_id, fe.entry_type, fe.page_number, fe.chapter_label, fe.anchor_text,
			fe.comment_body, fe.tag, fe.question_id, fe.position, fe.created_at, fe.updated_at,
			fer.author_reaction, fer.author_comment, fs.book_id
		FROM feedback_entries fe
		JOIN feedback_submissions fs ON fs.id = fe.submission_id
		JOIN books b ON b.id = fs.book_id
		LEFT JOIN feedback_entry_responses fer ON fer.feedback_entry_id = fe.id
		WHERE fe.id = ? AND fs.status = 'submitted' AND b.owner_user_id = ?
	`, entryID, authorUserID).Scan(
		&entry.ID,
		&entry.SubmissionID,
		&entry.EntryType,
		&entry.PageNumber,
		&entry.ChapterLabel,
		&entry.AnchorText,
		&entry.CommentBody,
		&entry.Tag,
		&entry.QuestionID,
		&entry.Position,
		&entry.CreatedAt,
		&entry.UpdatedAt,
		&entry.AuthorReaction,
		&entry.AuthorComment,
		&bookID,
	)
	if err != nil {
		return nil, 0, err
	}
	return &entry, bookID, nil
}

func (app *application) upsertEntryResponse(entryID int64, reaction, comment string) error {
	now := time.Now().UTC()
	_, err := app.db.Exec(`
		INSERT INTO feedback_entry_responses (feedback_entry_id, author_reaction, author_comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(feedback_entry_id) DO UPDATE SET
			author_reaction = excluded.author_reaction,
			author_comment = excluded.author_comment,
			updated_at = excluded.updated_at
	`, entryID, nullIfEmpty(reaction), nullIfEmpty(comment), now, now)
	return err
}

func (app *application) submitFeedbackSubmission(submissionID int64) error {
	now := time.Now().UTC()
	_, err := app.db.Exec(`
		UPDATE feedback_submissions
		SET status = 'submitted', submitted_at = ?, updated_at = ?
		WHERE id = ? AND status = 'draft'
	`, now, now, submissionID)
	return err
}

func (app *application) listSubmittedFeedbackForBook(bookID int64) ([]SubmittedFeedbackGroup, error) {
	rows, err := app.db.Query(`
		SELECT fs.id, fs.reviewer_user_id, fs.submitted_at, u.name, u.email
		FROM feedback_submissions fs
		JOIN users u ON u.id = fs.reviewer_user_id
		WHERE fs.book_id = ? AND fs.status = 'submitted'
		ORDER BY fs.submitted_at DESC, fs.id DESC
	`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []SubmittedFeedbackGroup
	for rows.Next() {
		var group SubmittedFeedbackGroup
		var submittedAt sql.NullTime
		if err := rows.Scan(&group.SubmissionID, &group.ReviewerUserID, &submittedAt, &group.ReviewerName, &group.ReviewerEmail); err != nil {
			return nil, err
		}
		if submittedAt.Valid {
			group.SubmittedAt = submittedAt.Time
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range groups {
		entries, err := app.listFeedbackEntries(groups[i].SubmissionID)
		if err != nil {
			return nil, err
		}
		groups[i].Entries = entries
	}

	return groups, nil
}

func (app *application) listSubmittedFeedbackForReviewer(bookID, reviewerUserID int64) ([]SubmittedFeedbackGroup, error) {
	rows, err := app.db.Query(`
		SELECT fs.id, fs.reviewer_user_id, fs.submitted_at, u.name, u.email
		FROM feedback_submissions fs
		JOIN users u ON u.id = fs.reviewer_user_id
		WHERE fs.book_id = ? AND fs.reviewer_user_id = ? AND fs.status = 'submitted'
		ORDER BY fs.submitted_at DESC, fs.id DESC
	`, bookID, reviewerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []SubmittedFeedbackGroup
	for rows.Next() {
		var group SubmittedFeedbackGroup
		var submittedAt sql.NullTime
		if err := rows.Scan(&group.SubmissionID, &group.ReviewerUserID, &submittedAt, &group.ReviewerName, &group.ReviewerEmail); err != nil {
			return nil, err
		}
		if submittedAt.Valid {
			group.SubmittedAt = submittedAt.Time
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range groups {
		entries, err := app.listFeedbackEntries(groups[i].SubmissionID)
		if err != nil {
			return nil, err
		}
		groups[i].Entries = entries
	}

	return groups, nil
}

func (app *application) bootstrapAdmin(email string) error {
	res, err := app.db.Exec(`UPDATE users SET is_admin = 1, updated_at = ? WHERE email = ?`, time.Now().UTC(), email)
	if err != nil {
		return err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("no user found for BOOTSTRAP_ADMIN_EMAIL=%s", email)
	}

	return nil
}

func fallbackName(name, email string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}

	parts := strings.Split(email, "@")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}

	return "Reader"
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func nullIfEmpty(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func currentNullString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func (app *application) absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func safeRedirectPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	return value
}

func (app *application) afterAuthRedirect(nextPath string) string {
	if safe := safeRedirectPath(nextPath); safe != "" {
		return safe
	}
	return "/app"
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (app *application) expireCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   app.sessionCookieSecure,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func newGoogleOAuthConfig() *oauth2.Config {
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
	redirectURL := strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL"))

	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil
	}

	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"openid", "email", "profile"},
	}
}

func fetchGoogleProfile(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (googleProfile, error) {
	client := cfg.Client(ctx, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	if err != nil {
		return googleProfile{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return googleProfile{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return googleProfile{}, fmt.Errorf("unexpected google profile status: %d", resp.StatusCode)
	}

	var profile googleProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return googleProfile{}, err
	}

	return profile, nil
}

func fsSub(dir string) (fs.FS, error) {
	return fs.Sub(assets, dir)
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid .env line: %q", line)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("invalid .env line: %q", line)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}

	return scanner.Err()
}
