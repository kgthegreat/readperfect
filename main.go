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
	"unicode"

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
	flashCookieName      = "readperfect_flash"
	defaultSessionLength = 24 * time.Hour * 14
)

var (
	errInvalidCredentials = errors.New("invalid credentials")
	errEmailInUse         = errors.New("email already in use")
)

type contextKey string

const (
	userContextKey  contextKey = "current_user"
	flashContextKey contextKey = "flash_message"
)

type application struct {
	db                  *sql.DB
	templateCache       map[string]*template.Template
	staticFS            http.Handler
	sessionCookieSecure bool
	googleOAuth         *oauth2.Config
	googleLoginEnabled  bool
}

type templateData struct {
	CurrentUser         *User
	GoogleLoginEnabled  bool
	Flash               string
	Form                map[string]string
	Errors              map[string]string
	Stats               dashboardStats
	Books               []Book
	ReviewerAssignments []ReviewerAssignment
	Book                *Book
	CanDeleteBook       bool
	Questions           []AuthorQuestion
	Invitations         []ReviewInvitation
	GeneratedInviteURL  string
	Invitation          *ReviewInvitation
	InviteBook          *Book
	NextPath            string
	InviteAccepted      bool
	ReviewBook          *Book
	ReviewerSubmission  *FeedbackSubmission
	ReviewChapters      []ReviewChapter
	ActiveReviewChapter *ReviewChapter
	SubmittedFeedback   []SubmittedFeedbackGroup
	AdminStats          adminStats
	AdminUsers          []AdminUserRow
	AdminBooks          []AdminBookRow
}

type dashboardStats struct {
	BooksOwned         int
	PendingInvitations int
	SubmittedFeedback  int
}

type adminStats struct {
	Users             int
	Books             int
	Reviewers         int
	LatestSubmissions int
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
	PublicID    string
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
	PublicID       string
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

type ReviewChapter struct {
	ID             int64
	SubmissionID   int64
	Label          string
	NoteAnchorText sql.NullString
	NoteBody       sql.NullString
	Position       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	AuthorReaction sql.NullString
	AuthorComment  sql.NullString
	Pages          []ReviewPage
}

type ReviewPage struct {
	ID             int64
	ChapterID      int64
	PageNumber     int
	AnchorText     sql.NullString
	CommentBody    string
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
	Chapters       []ReviewChapter
}

type ReviewerAssignment struct {
	Book        Book
	SubmittedAt sql.NullTime
}

type AdminUserRow struct {
	ID            int64
	Email         string
	Name          string
	IsAdmin       bool
	BooksOwned    int
	ReviewerBooks int
	CreatedAt     time.Time
}

type AdminBookRow struct {
	ID                int64
	Title             string
	AuthorName        string
	OwnerName         string
	OwnerEmail        string
	Status            string
	ReviewerCount     int
	InvitationCount   int
	LatestSubmissions int
	CreatedAt         time.Time
}

func (b Book) PublicPath() string {
	return "/books/" + bookRouteKey(b.Title, b.PublicID)
}

func (s FeedbackSubmission) PublicPath() string {
	return "/reviews/" + s.PublicID
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
		googleLoginEnabled:  googleLoginEnabled(),
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
	mux.HandleFunc("/privacy", app.privacy)
	mux.HandleFunc("/terms", app.terms)
	mux.HandleFunc("/contact", app.contact)
	mux.HandleFunc("/login", app.login)
	mux.HandleFunc("/signup", app.signup)
	mux.HandleFunc("/logout", app.logout)
	mux.HandleFunc("/auth/google/start", app.googleStart)
	mux.HandleFunc("/auth/google/callback", app.googleCallback)
	mux.HandleFunc("/invites/", app.invitesRouter)
	mux.Handle("/review-chapters/", app.requireAuth(http.HandlerFunc(app.reviewChaptersRouter)))
	mux.Handle("/review-pages/", app.requireAuth(http.HandlerFunc(app.reviewPagesRouter)))
	mux.Handle("/app", app.requireAuth(http.HandlerFunc(app.dashboard)))
	mux.Handle("/reviews", app.requireAuth(http.HandlerFunc(app.reviewsIndex)))
	mux.Handle("/reviews/open", app.requireAuth(http.HandlerFunc(app.openReviewWorkspace)))
	mux.Handle("/admin", app.requireAdmin(http.HandlerFunc(app.adminDashboard)))
	mux.Handle("/admin/users", app.requireAdmin(http.HandlerFunc(app.adminUsers)))
	mux.Handle("/admin/books", app.requireAdmin(http.HandlerFunc(app.adminBooks)))
	mux.Handle("/reviews/", app.requireAuth(http.HandlerFunc(app.reviewsRouter)))
	mux.Handle("/books/new", app.requireAuth(http.HandlerFunc(app.newBook)))
	mux.Handle("/books", app.requireAuth(http.HandlerFunc(app.createBook)))
	mux.Handle("/books/", app.requireAuth(http.HandlerFunc(app.booksRouter)))

	return app.loadFlash(app.loadUser(mux))
}

func (app *application) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := app.newTemplateData(r)
	app.render(w, http.StatusOK, "home", data)
}

func (app *application) privacy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/privacy" {
		http.NotFound(w, r)
		return
	}

	app.render(w, http.StatusOK, "privacy", app.newTemplateData(r))
}

func (app *application) terms(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/terms" {
		http.NotFound(w, r)
		return
	}

	app.render(w, http.StatusOK, "terms", app.newTemplateData(r))
}

func (app *application) contact(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/contact" {
		http.NotFound(w, r)
		return
	}

	app.render(w, http.StatusOK, "contact", app.newTemplateData(r))
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

func (app *application) reviewsIndex(w http.ResponseWriter, r *http.Request) {
	user := app.currentUser(r)
	if user == nil {
		log.Printf("reviewsIndex: no current user path=%s", r.URL.Path)
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	log.Printf("reviewsIndex: loading reviewer assignments user_id=%d email=%s", user.ID, user.Email)
	assignments, err := app.listReviewerAssignments(user.ID)
	if err != nil {
		log.Printf("reviewsIndex: listReviewerAssignments failed user_id=%d err=%v", user.ID, err)
		http.Error(w, "could not load reviewer assignments", http.StatusInternalServerError)
		return
	}
	log.Printf("reviewsIndex: loaded reviewer assignments user_id=%d count=%d", user.ID, len(assignments))

	data := app.newTemplateData(r)
	data.ReviewerAssignments = assignments
	app.render(w, http.StatusOK, "reviews_index", data)
}

func (app *application) openReviewWorkspace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := app.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	bookPublicID := parseBookPublicID(r.FormValue("book_public_id"))
	if bookPublicID == "" {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}

	book, err := app.getBookForReviewerByPublicID(bookPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	submission, _, err := app.ensureDraftSubmission(book.ID, user.ID)
	if err != nil {
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, submission.PublicPath(), http.StatusSeeOther)
}

func (app *application) adminDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := app.adminStats()
	if err != nil {
		http.Error(w, "could not load admin dashboard", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.AdminStats = stats
	app.render(w, http.StatusOK, "admin", data)
}

func (app *application) adminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := app.listAdminUsers()
	if err != nil {
		http.Error(w, "could not load users", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.AdminUsers = users
	app.render(w, http.StatusOK, "admin_users", data)
}

func (app *application) adminBooks(w http.ResponseWriter, r *http.Request) {
	books, err := app.listAdminBooks()
	if err != nil {
		http.Error(w, "could not load books", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.AdminBooks = books
	app.render(w, http.StatusOK, "admin_books", data)
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

	app.redirectWithFlash(w, r, "/app", "Book created.")
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

	bookPublicID := parseBookPublicID(parts[0])
	if bookPublicID == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.showBook(w, r, bookPublicID)
	case len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost:
		app.deleteBook(w, r, bookPublicID)
	case len(parts) == 2 && parts[1] == "questions" && r.Method == http.MethodPost:
		app.createQuestion(w, r, bookPublicID)
	case len(parts) == 2 && parts[1] == "invitations" && r.Method == http.MethodPost:
		app.createInvitation(w, r, bookPublicID)
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
		http.Redirect(w, r, "/reviews", http.StatusSeeOther)
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	reviewPublicID := strings.TrimSpace(parts[0])
	if reviewPublicID == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		app.showReviewerWorkspace(w, r, reviewPublicID)
	case len(parts) == 2 && parts[1] == "chapters" && r.Method == http.MethodPost:
		app.createReviewChapter(w, r, reviewPublicID)
	case len(parts) == 2 && parts[1] == "submit" && r.Method == http.MethodPost:
		app.submitReviewerDraft(w, r, reviewPublicID)
	default:
		http.NotFound(w, r)
	}
}

func (app *application) reviewChaptersRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/review-chapters/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	chapterID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case parts[1] == "note" && r.Method == http.MethodPost:
		app.saveReviewChapterNote(w, r, chapterID)
	case parts[1] == "pages" && r.Method == http.MethodPost:
		app.createReviewPage(w, r, chapterID)
	case parts[1] == "respond" && r.Method == http.MethodPost:
		app.respondToChapter(w, r, chapterID)
	default:
		http.NotFound(w, r)
	}
}

func (app *application) reviewPagesRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/review-pages/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	pageID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if parts[1] == "respond" && r.Method == http.MethodPost {
		app.respondToPage(w, r, pageID)
		return
	}

	http.NotFound(w, r)
}

func (app *application) showBook(w http.ResponseWriter, r *http.Request, bookPublicID string) {
	user := app.currentUser(r)
	book, err := app.getBookForOwnerByPublicID(bookPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load book", http.StatusInternalServerError)
		return
	}

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
		log.Printf("showBook submitted feedback book=%d err=%v", book.ID, err)
		http.Error(w, "could not load submitted feedback", http.StatusInternalServerError)
		return
	}
	canDeleteBook, err := app.canDeleteBook(book.ID, user.ID)
	if err != nil {
		http.Error(w, "could not determine delete status", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Book = book
	data.CanDeleteBook = canDeleteBook
	data.Questions = questions
	data.Invitations = invitations
	data.SubmittedFeedback = submittedFeedback
	app.render(w, http.StatusOK, "book_show", data)
}

func (app *application) deleteBook(w http.ResponseWriter, r *http.Request, bookPublicID string) {
	user := app.currentUser(r)
	book, err := app.getBookForOwnerByPublicID(bookPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load book", http.StatusInternalServerError)
		return
	}

	canDeleteBook, err := app.canDeleteBook(book.ID, user.ID)
	if err != nil {
		http.Error(w, "could not determine delete status", http.StatusInternalServerError)
		return
	}
	if !canDeleteBook {
		app.redirectWithFlash(w, r, book.PublicPath(), "This book cannot be deleted after reviewer work has started.")
		return
	}

	if err := app.deleteBookForOwner(book.ID, user.ID); err != nil {
		http.Error(w, "could not delete book", http.StatusInternalServerError)
		return
	}

	app.redirectWithFlash(w, r, "/app", "Book deleted.")
}

func (app *application) createQuestion(w http.ResponseWriter, r *http.Request, bookPublicID string) {
	user := app.currentUser(r)
	book, err := app.getBookForOwnerByPublicID(bookPublicID, user.ID)
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

	if err := app.insertQuestion(book.ID, question); err != nil {
		http.Error(w, "could not save question", http.StatusInternalServerError)
		return
	}

	app.redirectWithFlash(w, r, book.PublicPath(), "Question added.")
}

func (app *application) createInvitation(w http.ResponseWriter, r *http.Request, bookPublicID string) {
	user := app.currentUser(r)
	book, err := app.getBookForOwnerByPublicID(bookPublicID, user.ID)
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

	if err := app.insertInvitation(book.ID, user.ID, email, inviteToken); err != nil {
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
	canDeleteBook, err := app.canDeleteBook(book.ID, book.OwnerUserID)
	if err != nil {
		http.Error(w, "could not determine delete status", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.Book = book
	data.CanDeleteBook = canDeleteBook
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

	draft, _, err := app.ensureDraftSubmission(invitation.BookID, user.ID)
	if err != nil {
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	app.redirectWithFlash(w, r, draft.PublicPath(), "Invite accepted.")
}

func (app *application) showReviewerWorkspace(w http.ResponseWriter, r *http.Request, reviewPublicID string) {
	user := app.currentUser(r)
	book, submission, err := app.getDraftSubmissionByPublicID(reviewPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	app.renderReviewWorkspace(w, r, book, submission, parseInt64(r.URL.Query().Get("chapter")), nil, nil, http.StatusOK)
}

func (app *application) renderReviewWorkspace(w http.ResponseWriter, r *http.Request, book *Book, submission *FeedbackSubmission, activeChapterID int64, errorsMap map[string]string, formValues map[string]string, status int) {
	chapters, err := app.listReviewChapters(submission.ID)
	if err != nil {
		http.Error(w, "could not load review chapters", http.StatusInternalServerError)
		return
	}
	questions, err := app.listQuestionsForBook(book.ID)
	if err != nil {
		http.Error(w, "could not load author questions", http.StatusInternalServerError)
		return
	}
	submittedFeedback, err := app.listSubmittedFeedbackForReviewer(book.ID, submission.ReviewerUserID)
	if err != nil {
		http.Error(w, "could not load submitted feedback", http.StatusInternalServerError)
		return
	}

	data := app.newTemplateData(r)
	data.ReviewBook = book
	data.ReviewerSubmission = submission
	data.Questions = questions
	data.ReviewChapters = chapters
	data.ActiveReviewChapter = selectActiveReviewChapter(chapters, activeChapterID)
	data.SubmittedFeedback = submittedFeedback
	data.Errors = errorsMap
	data.Form = formValues
	if data.Form == nil {
		data.Form = map[string]string{}
	}
	if data.ActiveReviewChapter != nil && strings.TrimSpace(data.Form["page_number"]) == "" {
		data.Form["page_number"] = strconv.Itoa(nextSuggestedPageNumber(data.ActiveReviewChapter.Pages))
	}
	if app.isHTMX(r) {
		app.renderNamed(w, status, "review_show", "review_workspace", data)
		return
	}
	app.render(w, status, "review_show", data)
}

func (app *application) createReviewChapter(w http.ResponseWriter, r *http.Request, reviewPublicID string) {
	user := app.currentUser(r)
	book, submission, err := app.getDraftSubmissionByPublicID(reviewPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	chapterLabel := strings.TrimSpace(r.FormValue("chapter_label"))
	if chapterLabel == "" {
		app.renderReviewWorkspace(w, r, book, submission, 0, map[string]string{"new_chapter": "Enter a chapter label."}, map[string]string{"new_chapter_label": chapterLabel}, http.StatusUnprocessableEntity)
		return
	}
	if len(chapterLabel) > 200 {
		app.renderReviewWorkspace(w, r, book, submission, 0, map[string]string{"new_chapter": "Chapter label is too long."}, map[string]string{"new_chapter_label": chapterLabel}, http.StatusUnprocessableEntity)
		return
	}
	exists, err := app.reviewChapterLabelExists(submission.ID, chapterLabel)
	if err != nil {
		http.Error(w, "could not validate chapter label", http.StatusInternalServerError)
		return
	}
	if exists {
		app.renderReviewWorkspace(w, r, book, submission, 0, map[string]string{"new_chapter": "That chapter already exists in this review."}, map[string]string{"new_chapter_label": chapterLabel}, http.StatusUnprocessableEntity)
		return
	}

	chapterID, err := app.insertReviewChapter(submission.ID, chapterLabel)
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			app.renderReviewWorkspace(w, r, book, submission, 0, map[string]string{"new_chapter": "That chapter already exists in this review."}, map[string]string{"new_chapter_label": chapterLabel}, http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "could not create chapter", http.StatusInternalServerError)
		return
	}

	if app.isHTMX(r) {
		app.renderReviewWorkspace(w, r, book, submission, chapterID, nil, nil, http.StatusOK)
		return
	}
	app.redirectWithFlash(w, r, fmt.Sprintf("%s?chapter=%d", submission.PublicPath(), chapterID), "Chapter added.")
}

func (app *application) saveReviewChapterNote(w http.ResponseWriter, r *http.Request, chapterID int64) {
	user := app.currentUser(r)
	chapter, book, submission, err := app.getDraftReviewChapterForReviewer(chapterID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load chapter", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	anchorText := strings.TrimSpace(r.FormValue("anchor_text"))
	commentBody := strings.TrimSpace(r.FormValue("comment_body"))
	if len(commentBody) > 4000 {
		app.renderReviewWorkspace(w, r, book, submission, chapter.ID, map[string]string{"chapter_note": "Chapter note is too long."}, map[string]string{"chapter_anchor_text": anchorText, "chapter_comment_body": commentBody}, http.StatusUnprocessableEntity)
		return
	}

	if err := app.updateReviewChapterNote(chapterID, anchorText, commentBody); err != nil {
		http.Error(w, "could not save chapter note", http.StatusInternalServerError)
		return
	}

	if app.isHTMX(r) {
		app.renderReviewWorkspace(w, r, book, submission, chapter.ID, nil, nil, http.StatusOK)
		return
	}
	app.redirectWithFlash(w, r, fmt.Sprintf("%s?chapter=%d", submission.PublicPath(), chapter.ID), "Chapter note saved.")
}

func (app *application) createReviewPage(w http.ResponseWriter, r *http.Request, chapterID int64) {
	user := app.currentUser(r)
	chapter, book, submission, err := app.getDraftReviewChapterForReviewer(chapterID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load chapter", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	pageValue := strings.TrimSpace(r.FormValue("page_number"))
	anchorText := strings.TrimSpace(r.FormValue("anchor_text"))
	commentBody := strings.TrimSpace(r.FormValue("comment_body"))

	pageNumber, err := strconv.Atoi(pageValue)
	if err != nil || pageNumber <= 0 {
		app.renderReviewWorkspace(w, r, book, submission, chapter.ID, map[string]string{"page_number": "Enter a valid page number."}, map[string]string{"page_number": pageValue, "page_anchor_text": anchorText, "page_comment_body": commentBody}, http.StatusUnprocessableEntity)
		return
	}
	if commentBody == "" {
		app.renderReviewWorkspace(w, r, book, submission, chapter.ID, map[string]string{"page_note": "Enter the page note."}, map[string]string{"page_number": pageValue, "page_anchor_text": anchorText, "page_comment_body": commentBody}, http.StatusUnprocessableEntity)
		return
	}
	if err := app.insertReviewPage(chapterID, pageNumber, anchorText, commentBody); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			app.renderReviewWorkspace(w, r, book, submission, chapter.ID, map[string]string{"page_number": "That page already exists in this chapter."}, map[string]string{"page_number": pageValue, "page_anchor_text": anchorText, "page_comment_body": commentBody}, http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "could not create page note", http.StatusInternalServerError)
		return
	}

	if app.isHTMX(r) {
		app.renderReviewWorkspace(w, r, book, submission, chapter.ID, nil, nil, http.StatusOK)
		return
	}
	app.redirectWithFlash(w, r, fmt.Sprintf("%s?chapter=%d", submission.PublicPath(), chapter.ID), "Page note saved.")
}

func (app *application) submitReviewerDraft(w http.ResponseWriter, r *http.Request, reviewPublicID string) {
	user := app.currentUser(r)
	_, submission, err := app.getDraftSubmissionByPublicID(reviewPublicID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load review workspace", http.StatusInternalServerError)
		return
	}
	if ok, err := app.hasDraftReviewContent(submission.ID); err != nil {
		http.Error(w, "could not inspect draft content", http.StatusInternalServerError)
		return
	} else if !ok {
		app.redirectWithFlash(w, r, submission.PublicPath(), "Add at least one note before submitting.")
		return
	}

	if err := app.submitFeedbackSubmission(submission.ID); err != nil {
		http.Error(w, "could not submit feedback", http.StatusInternalServerError)
		return
	}

	app.redirectWithFlash(w, r, submission.PublicPath(), "Feedback submitted. You can keep working on this draft.")
}

func (app *application) respondToChapter(w http.ResponseWriter, r *http.Request, chapterID int64) {
	user := app.currentUser(r)
	chapter, bookID, err := app.getSubmittedReviewChapterForAuthor(chapterID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load chapter", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	bookPath, err := app.bookPathByID(bookID)
	if err != nil {
		http.Error(w, "could not load book route", http.StatusInternalServerError)
		return
	}

	reaction, comment, ok := resolveAuthorResponse(r, chapter.AuthorReaction, chapter.AuthorComment)
	if !ok {
		app.redirectWithFlash(w, r, bookPath, "Unsupported response.")
		return
	}
	if len(comment) > 2000 {
		app.redirectWithFlash(w, r, bookPath, "Comment is too long.")
		return
	}
	if err := app.updateReviewChapterResponse(chapterID, reaction, comment); err != nil {
		http.Error(w, "could not save response", http.StatusInternalServerError)
		return
	}
	app.redirectWithFlash(w, r, bookPath, "Response saved.")
}

func (app *application) respondToPage(w http.ResponseWriter, r *http.Request, pageID int64) {
	user := app.currentUser(r)
	page, bookID, err := app.getSubmittedReviewPageForAuthor(pageID, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load page note", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	bookPath, err := app.bookPathByID(bookID)
	if err != nil {
		http.Error(w, "could not load book route", http.StatusInternalServerError)
		return
	}

	reaction, comment, ok := resolveAuthorResponse(r, page.AuthorReaction, page.AuthorComment)
	if !ok {
		app.redirectWithFlash(w, r, bookPath, "Unsupported response.")
		return
	}
	if len(comment) > 2000 {
		app.redirectWithFlash(w, r, bookPath, "Comment is too long.")
		return
	}
	if err := app.updateReviewPageResponse(pageID, reaction, comment); err != nil {
		http.Error(w, "could not save response", http.StatusInternalServerError)
		return
	}
	app.redirectWithFlash(w, r, bookPath, "Response saved.")
}

func resolveAuthorResponse(r *http.Request, currentReaction sql.NullString, currentComment sql.NullString) (string, string, bool) {
	action := strings.TrimSpace(r.FormValue("action"))
	reaction := strings.TrimSpace(r.FormValue("reaction"))
	comment := strings.TrimSpace(r.FormValue("comment"))

	if reaction != "" && reaction != "noted" {
		return "", "", false
	}

	switch action {
	case "toggle_reaction":
		if reaction == "" {
			return "", "", false
		}
		if currentReaction.Valid && currentReaction.String == reaction {
			reaction = ""
		}
		comment = currentNullString(currentComment)
	case "save_comment":
		reaction = currentNullString(currentReaction)
	default:
		return "", "", false
	}

	return reaction, comment, true
}

func (app *application) googleStart(w http.ResponseWriter, r *http.Request) {
	if app.googleOAuth == nil || !app.googleLoginEnabled {
		http.NotFound(w, r)
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
	if app.googleOAuth == nil || !app.googleLoginEnabled {
		http.NotFound(w, r)
		return
	}

	stateCookie, err := r.Cookie(googleStateCookie)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		app.redirectWithFlash(w, r, "/login", "Google sign-in could not be completed. Please try again.")
		return
	}
	app.expireCookie(w, googleStateCookie)

	code := r.URL.Query().Get("code")
	if code == "" {
		app.redirectWithFlash(w, r, "/login", "Google sign-in could not be completed. Please try again.")
		return
	}

	token, err := app.googleOAuth.Exchange(r.Context(), code)
	if err != nil {
		app.redirectWithFlash(w, r, "/login", "Google sign-in could not be completed. Please try again.")
		return
	}

	profile, err := fetchGoogleProfile(r.Context(), app.googleOAuth, token)
	if err != nil {
		app.redirectWithFlash(w, r, "/login", "Google sign-in could not be completed. Please try again.")
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
			app.redirectWithFlash(w, r, "/login", "Sign in to continue.")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (app *application) requireAdmin(next http.Handler) http.Handler {
	return app.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := app.currentUser(r)
		if user == nil || !user.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (app *application) loadFlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(flashCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
		if err != nil {
			app.expireCookie(w, flashCookieName)
			next.ServeHTTP(w, r)
			return
		}

		app.expireCookie(w, flashCookieName)
		ctx := context.WithValue(r.Context(), flashContextKey, string(raw))
		next.ServeHTTP(w, r.WithContext(ctx))
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

func (app *application) currentFlash(r *http.Request) string {
	flash, ok := r.Context().Value(flashContextKey).(string)
	if !ok {
		return ""
	}

	return flash
}

func (app *application) newTemplateData(r *http.Request) *templateData {
	return &templateData{
		CurrentUser:        app.currentUser(r),
		GoogleLoginEnabled: app.googleOAuth != nil && app.googleLoginEnabled,
		Flash:              app.currentFlash(r),
		Form:               map[string]string{},
		Errors:             map[string]string{},
	}
}

func (app *application) redirectWithFlash(w http.ResponseWriter, r *http.Request, path string, message string) {
	app.setFlash(w, message)
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (app *application) setFlash(w http.ResponseWriter, message string) {
	if strings.TrimSpace(message) == "" {
		app.expireCookie(w, flashCookieName)
		return
	}

	encoded := base64.RawURLEncoding.EncodeToString([]byte(message))
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   app.sessionCookieSecure,
		MaxAge:   60,
	})
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

func (app *application) renderNamed(w http.ResponseWriter, status int, page string, name string, data *templateData) {
	tmpl, ok := app.templateCache[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s:%s: %v", page, name, err)
	}
}

func (app *application) isHTMX(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

func newTemplateCache() (map[string]*template.Template, error) {
	pages := []string{"home", "login", "signup", "dashboard", "reviews_index", "book_new", "book_show", "invite_show", "review_show", "privacy", "terms", "contact", "admin", "admin_users", "admin_books"}
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
			public_id TEXT,
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
		`CREATE TABLE IF NOT EXISTS review_drafts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			public_id TEXT,
			book_id INTEGER NOT NULL,
			reviewer_user_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(book_id, reviewer_user_id),
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE,
			FOREIGN KEY (reviewer_user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS review_draft_chapters (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			draft_id INTEGER NOT NULL,
			label TEXT NOT NULL,
			note_anchor_text TEXT,
			note_body TEXT,
			position INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (draft_id) REFERENCES review_drafts(id) ON DELETE CASCADE
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_draft_chapters_draft_label
			ON review_draft_chapters(draft_id, label COLLATE NOCASE);`,
		`CREATE TABLE IF NOT EXISTS review_draft_pages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chapter_id INTEGER NOT NULL,
			page_number INTEGER NOT NULL,
			anchor_text TEXT,
			comment_body TEXT NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(chapter_id, page_number),
			FOREIGN KEY (chapter_id) REFERENCES review_draft_chapters(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS review_submissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			public_id TEXT,
			book_id INTEGER NOT NULL,
			reviewer_user_id INTEGER NOT NULL,
			submitted_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL,
			FOREIGN KEY (book_id) REFERENCES books(id) ON DELETE CASCADE,
			FOREIGN KEY (reviewer_user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS review_submission_chapters (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			submission_id INTEGER NOT NULL,
			label TEXT NOT NULL,
			note_anchor_text TEXT,
			note_body TEXT,
			position INTEGER NOT NULL DEFAULT 0,
			author_reaction TEXT,
			author_comment TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY (submission_id) REFERENCES review_submissions(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS review_submission_pages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chapter_id INTEGER NOT NULL,
			page_number INTEGER NOT NULL,
			anchor_text TEXT,
			comment_body TEXT NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			author_reaction TEXT,
			author_comment TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(chapter_id, page_number),
			FOREIGN KEY (chapter_id) REFERENCES review_submission_chapters(id) ON DELETE CASCADE
		);`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	columnMigrations := []struct {
		table   string
		column  string
		alterSQL string
	}{
		{"books", "public_id", `ALTER TABLE books ADD COLUMN public_id TEXT`},
		{"review_drafts", "public_id", `ALTER TABLE review_drafts ADD COLUMN public_id TEXT`},
		{"review_submissions", "public_id", `ALTER TABLE review_submissions ADD COLUMN public_id TEXT`},
	}
	for _, migration := range columnMigrations {
		if err := addColumnIfMissing(db, migration.table, migration.column, migration.alterSQL); err != nil {
			return err
		}
	}

	if err := backfillPublicIDs(db, "books", "public_id", "bk_"); err != nil {
		return err
	}
	if err := backfillPublicIDs(db, "review_drafts", "public_id", "rv_"); err != nil {
		return err
	}
	if err := backfillPublicIDs(db, "review_submissions", "public_id", "rs_"); err != nil {
		return err
	}

	indexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_books_public_id ON books(public_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_drafts_public_id ON review_drafts(public_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_submissions_public_id ON review_submissions(public_id);`,
	}
	for _, stmt := range indexes {
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
		FROM review_submissions rs
		JOIN books b ON b.id = rs.book_id
		WHERE b.owner_user_id = ?
	`, userID).Scan(&stats.SubmittedFeedback); err != nil {
		return stats, err
	}
	return stats, nil
}

func (app *application) adminStats() (adminStats, error) {
	stats := adminStats{}

	if err := app.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&stats.Users); err != nil {
		return stats, err
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&stats.Books); err != nil {
		return stats, err
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM book_reviewers`).Scan(&stats.Reviewers); err != nil {
		return stats, err
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM review_submissions`).Scan(&stats.LatestSubmissions); err != nil {
		return stats, err
	}

	return stats, nil
}

func (app *application) listAdminUsers() ([]AdminUserRow, error) {
	rows, err := app.db.Query(`
		SELECT
			u.id, u.email, u.name, u.is_admin, u.created_at,
			(SELECT COUNT(*) FROM books b WHERE b.owner_user_id = u.id) AS books_owned,
			(SELECT COUNT(*) FROM book_reviewers br WHERE br.user_id = u.id) AS reviewer_books
		FROM users u
		ORDER BY u.created_at DESC, u.id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []AdminUserRow
	for rows.Next() {
		var user AdminUserRow
		if err := rows.Scan(&user.ID, &user.Email, &user.Name, &user.IsAdmin, &user.CreatedAt, &user.BooksOwned, &user.ReviewerBooks); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (app *application) listAdminBooks() ([]AdminBookRow, error) {
	rows, err := app.db.Query(`
		SELECT
			b.id, b.title, b.author_name, u.name, u.email, b.status, b.created_at,
			(SELECT COUNT(*) FROM book_reviewers br WHERE br.book_id = b.id) AS reviewer_count,
			(SELECT COUNT(*) FROM review_invitations ri WHERE ri.book_id = b.id) AS invitation_count,
			(SELECT COUNT(*) FROM review_submissions rs WHERE rs.book_id = b.id) AS latest_submissions
		FROM books b
		JOIN users u ON u.id = b.owner_user_id
		ORDER BY b.created_at DESC, b.id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []AdminBookRow
	for rows.Next() {
		var book AdminBookRow
		if err := rows.Scan(
			&book.ID,
			&book.Title,
			&book.AuthorName,
			&book.OwnerName,
			&book.OwnerEmail,
			&book.Status,
			&book.CreatedAt,
			&book.ReviewerCount,
			&book.InvitationCount,
			&book.LatestSubmissions,
		); err != nil {
			return nil, err
		}
		books = append(books, book)
	}
	return books, rows.Err()
}

func (app *application) listBooksByOwner(userID int64) ([]Book, error) {
	rows, err := app.db.Query(`
		SELECT id, public_id, owner_user_id, title, author_name, isbn, cover_url, description, status, created_at, updated_at
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
			&book.PublicID,
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

func (app *application) listReviewerAssignments(userID int64) ([]ReviewerAssignment, error) {
	log.Printf("listReviewerAssignments: start user_id=%d", userID)
	rows, err := app.db.Query(`
		SELECT
			b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at,
			(
				SELECT MAX(rs.submitted_at)
				FROM review_submissions rs
				WHERE rs.book_id = b.id AND rs.reviewer_user_id = br.user_id
			) AS last_submitted_at
		FROM book_reviewers br
		JOIN books b ON b.id = br.book_id
		WHERE br.user_id = ?
		ORDER BY b.created_at DESC
	`, userID)
	if err != nil {
		log.Printf("listReviewerAssignments: query failed user_id=%d err=%v", userID, err)
		return nil, err
	}
	defer rows.Close()

	var assignments []ReviewerAssignment
	for rows.Next() {
		var assignment ReviewerAssignment
		var isbn, coverURL, description sql.NullString
		var submittedAtRaw sql.NullString
		if err := rows.Scan(
			&assignment.Book.ID,
			&assignment.Book.PublicID,
			&assignment.Book.OwnerUserID,
			&assignment.Book.Title,
			&assignment.Book.AuthorName,
			&isbn,
			&coverURL,
			&description,
			&assignment.Book.Status,
			&assignment.Book.CreatedAt,
			&assignment.Book.UpdatedAt,
			&submittedAtRaw,
		); err != nil {
			log.Printf("listReviewerAssignments: scan failed user_id=%d err=%v", userID, err)
			return nil, err
		}
		assignment.Book.ISBN = isbn.String
		assignment.Book.CoverURL = coverURL.String
		assignment.Book.Description = description.String
		assignment.SubmittedAt = parseNullableTime(submittedAtRaw)
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		log.Printf("listReviewerAssignments: rows iteration failed user_id=%d err=%v", userID, err)
		return nil, err
	}

	log.Printf("listReviewerAssignments: success user_id=%d count=%d", userID, len(assignments))
	return assignments, nil
}

func (app *application) getBookForOwner(bookID, ownerUserID int64) (*Book, error) {
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT id, public_id, owner_user_id, title, author_name, isbn, cover_url, description, status, created_at, updated_at
		FROM books
		WHERE id = ? AND owner_user_id = ?
	`, bookID, ownerUserID).Scan(
		&book.ID,
		&book.PublicID,
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

func (app *application) getBookForOwnerByPublicID(publicID string, ownerUserID int64) (*Book, error) {
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT id, public_id, owner_user_id, title, author_name, isbn, cover_url, description, status, created_at, updated_at
		FROM books
		WHERE public_id = ? AND owner_user_id = ?
	`, publicID, ownerUserID).Scan(
		&book.ID,
		&book.PublicID,
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

func (app *application) canDeleteBook(bookID, ownerUserID int64) (bool, error) {
	var ownerExists bool
	if err := app.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1
			FROM books
			WHERE id = ? AND owner_user_id = ?
		)
	`, bookID, ownerUserID).Scan(&ownerExists); err != nil {
		return false, err
	}
	if !ownerExists {
		return false, sql.ErrNoRows
	}

	var hasReviews bool
	if err := app.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM review_drafts WHERE book_id = ?
			UNION ALL
			SELECT 1 FROM review_submissions WHERE book_id = ?
		)
	`, bookID, bookID).Scan(&hasReviews); err != nil {
		return false, err
	}

	return !hasReviews, nil
}

func (app *application) deleteBookForOwner(bookID, ownerUserID int64) error {
	result, err := app.db.Exec(`DELETE FROM books WHERE id = ? AND owner_user_id = ?`, bookID, ownerUserID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (app *application) bookPathByID(bookID int64) (string, error) {
	var book Book
	err := app.db.QueryRow(`SELECT public_id, title FROM books WHERE id = ?`, bookID).Scan(&book.PublicID, &book.Title)
	if err != nil {
		return "", err
	}
	return book.PublicPath(), nil
}

func (app *application) insertBook(ownerUserID int64, title, authorName, isbn, description string) (*Book, error) {
	now := time.Now().UTC()
	publicID, err := app.generateUniquePublicID("books", "public_id", "bk_")
	if err != nil {
		return nil, err
	}
	res, err := app.db.Exec(`
		INSERT INTO books (public_id, owner_user_id, title, author_name, isbn, description, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'draft', ?, ?)
	`, publicID, ownerUserID, title, authorName, nullIfEmpty(isbn), nullIfEmpty(description), now, now)
	if err != nil {
		return nil, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Book{
		ID:          id,
		PublicID:    publicID,
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
			b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
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
		&book.PublicID,
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
		SELECT b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM books b
		JOIN book_reviewers br ON br.book_id = b.id
		WHERE b.id = ? AND br.user_id = ?
	`, bookID, reviewerUserID).Scan(
		&book.ID,
		&book.PublicID,
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

func (app *application) getBookForReviewerByPublicID(publicID string, reviewerUserID int64) (*Book, error) {
	var book Book
	var isbn, coverURL, description sql.NullString
	err := app.db.QueryRow(`
		SELECT b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM books b
		JOIN book_reviewers br ON br.book_id = b.id
		WHERE b.public_id = ? AND br.user_id = ?
	`, publicID, reviewerUserID).Scan(
		&book.ID,
		&book.PublicID,
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
		SELECT id, public_id, book_id, reviewer_user_id, created_at, updated_at
		FROM review_drafts
		WHERE book_id = ? AND reviewer_user_id = ?
	`, bookID, reviewerUserID).Scan(
		&submission.ID,
		&submission.PublicID,
		&submission.BookID,
		&submission.ReviewerUserID,
		&submission.CreatedAt,
		&submission.UpdatedAt,
	)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		now := time.Now().UTC()
		publicID, err := app.generateUniquePublicID("review_drafts", "public_id", "rv_")
		if err != nil {
			return nil, nil, err
		}
		res, err := app.db.Exec(`
			INSERT INTO review_drafts (public_id, book_id, reviewer_user_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, publicID, bookID, reviewerUserID, now, now)
		if err != nil {
			return nil, nil, err
		}
		submissionID, err := res.LastInsertId()
		if err != nil {
			return nil, nil, err
		}
		submission = FeedbackSubmission{
			ID:             submissionID,
			PublicID:       publicID,
			BookID:         bookID,
			ReviewerUserID: reviewerUserID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
	default:
		return nil, nil, err
	}

	return &submission, nil, nil
}

func (app *application) getDraftSubmissionByPublicID(publicID string, reviewerUserID int64) (*Book, *FeedbackSubmission, error) {
	var book Book
	var submission FeedbackSubmission
	var isbn, coverURL, description sql.NullString

	err := app.db.QueryRow(`
		SELECT
			rd.id, rd.public_id, rd.book_id, rd.reviewer_user_id, rd.created_at, rd.updated_at,
			b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM review_drafts rd
		JOIN books b ON b.id = rd.book_id
		WHERE rd.public_id = ? AND rd.reviewer_user_id = ?
	`, publicID, reviewerUserID).Scan(
		&submission.ID, &submission.PublicID, &submission.BookID, &submission.ReviewerUserID, &submission.CreatedAt, &submission.UpdatedAt,
		&book.ID, &book.PublicID, &book.OwnerUserID, &book.Title, &book.AuthorName, &isbn, &coverURL, &description, &book.Status, &book.CreatedAt, &book.UpdatedAt,
	)
	if err != nil {
		return nil, nil, err
	}

	book.ISBN = isbn.String
	book.CoverURL = coverURL.String
	book.Description = description.String
	return &book, &submission, nil
}

func (app *application) listReviewChapters(submissionID int64) ([]ReviewChapter, error) {
	return app.listDraftReviewChapters(submissionID)
}

func (app *application) listDraftReviewChapters(draftID int64) ([]ReviewChapter, error) {
	rows, err := app.db.Query(`
		SELECT
			id, draft_id, label, note_anchor_text, note_body, position, created_at, updated_at
		FROM review_draft_chapters
		WHERE draft_id = ?
		ORDER BY position ASC, id ASC
	`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chapters []ReviewChapter
	for rows.Next() {
		var chapter ReviewChapter
		if err := rows.Scan(
			&chapter.ID,
			&chapter.SubmissionID,
			&chapter.Label,
			&chapter.NoteAnchorText,
			&chapter.NoteBody,
			&chapter.Position,
			&chapter.CreatedAt,
			&chapter.UpdatedAt,
		); err != nil {
			return nil, err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range chapters {
		pages, err := app.listDraftReviewPages(chapters[i].ID)
		if err != nil {
			return nil, err
		}
		chapters[i].Pages = pages
	}
	return chapters, nil
}

func (app *application) listReviewPages(chapterID int64) ([]ReviewPage, error) {
	return app.listDraftReviewPages(chapterID)
}

func (app *application) listDraftReviewPages(chapterID int64) ([]ReviewPage, error) {
	rows, err := app.db.Query(`
		SELECT id, chapter_id, page_number, anchor_text, comment_body, position, created_at, updated_at
		FROM review_draft_pages
		WHERE chapter_id = ?
		ORDER BY page_number ASC, position ASC, id ASC
	`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pages []ReviewPage
	for rows.Next() {
		var page ReviewPage
		if err := rows.Scan(
			&page.ID,
			&page.ChapterID,
			&page.PageNumber,
			&page.AnchorText,
			&page.CommentBody,
			&page.Position,
			&page.CreatedAt,
			&page.UpdatedAt,
		); err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, rows.Err()
}

func (app *application) listSubmittedReviewChapters(submissionID int64) ([]ReviewChapter, error) {
	rows, err := app.db.Query(`
		SELECT
			id, submission_id, label, note_anchor_text, note_body, position, created_at, updated_at,
			author_reaction, author_comment
		FROM review_submission_chapters
		WHERE submission_id = ?
		ORDER BY position ASC, id ASC
	`, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chapters []ReviewChapter
	for rows.Next() {
		var chapter ReviewChapter
		if err := rows.Scan(
			&chapter.ID,
			&chapter.SubmissionID,
			&chapter.Label,
			&chapter.NoteAnchorText,
			&chapter.NoteBody,
			&chapter.Position,
			&chapter.CreatedAt,
			&chapter.UpdatedAt,
			&chapter.AuthorReaction,
			&chapter.AuthorComment,
		); err != nil {
			return nil, err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range chapters {
		pages, err := app.listSubmittedReviewPages(chapters[i].ID)
		if err != nil {
			return nil, err
		}
		chapters[i].Pages = pages
	}
	return chapters, nil
}

func (app *application) listSubmittedReviewPages(chapterID int64) ([]ReviewPage, error) {
	rows, err := app.db.Query(`
		SELECT id, chapter_id, page_number, anchor_text, comment_body, position, created_at, updated_at,
		       author_reaction, author_comment
		FROM review_submission_pages
		WHERE chapter_id = ?
		ORDER BY page_number ASC, position ASC, id ASC
	`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pages []ReviewPage
	for rows.Next() {
		var page ReviewPage
		if err := rows.Scan(
			&page.ID,
			&page.ChapterID,
			&page.PageNumber,
			&page.AnchorText,
			&page.CommentBody,
			&page.Position,
			&page.CreatedAt,
			&page.UpdatedAt,
			&page.AuthorReaction,
			&page.AuthorComment,
		); err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, rows.Err()
}

func (app *application) insertReviewChapter(submissionID int64, label string) (int64, error) {
	var nextPosition int
	if err := app.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM review_draft_chapters WHERE draft_id = ?`, submissionID).Scan(&nextPosition); err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	res, err := app.db.Exec(`
		INSERT INTO review_draft_chapters (draft_id, label, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, submissionID, label, nextPosition, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (app *application) reviewChapterLabelExists(submissionID int64, label string) (bool, error) {
	var count int
	err := app.db.QueryRow(`
		SELECT COUNT(*)
		FROM review_draft_chapters
		WHERE draft_id = ? AND lower(trim(label)) = lower(trim(?))
	`, submissionID, label).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (app *application) updateReviewChapterNote(chapterID int64, anchorText, noteBody string) error {
	_, err := app.db.Exec(`
		UPDATE review_draft_chapters
		SET note_anchor_text = ?, note_body = ?, updated_at = ?
		WHERE id = ?
	`, nullIfEmpty(anchorText), nullIfEmpty(noteBody), time.Now().UTC(), chapterID)
	return err
}

func (app *application) insertReviewPage(chapterID int64, pageNumber int, anchorText, commentBody string) error {
	var nextPosition int
	if err := app.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM review_draft_pages WHERE chapter_id = ?`, chapterID).Scan(&nextPosition); err != nil {
		return err
	}

	now := time.Now().UTC()
	_, err := app.db.Exec(`
		INSERT INTO review_draft_pages (chapter_id, page_number, anchor_text, comment_body, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, chapterID, pageNumber, nullIfEmpty(anchorText), commentBody, nextPosition, now, now)
	return err
}

func (app *application) hasDraftReviewContent(submissionID int64) (bool, error) {
	var chapterCount int
	if err := app.db.QueryRow(`
		SELECT COUNT(*)
		FROM review_draft_chapters
		WHERE draft_id = ? AND (note_body IS NOT NULL OR EXISTS (SELECT 1 FROM review_draft_pages rp WHERE rp.chapter_id = review_draft_chapters.id))
	`, submissionID).Scan(&chapterCount); err != nil {
		return false, err
	}
	return chapterCount > 0, nil
}

func (app *application) getDraftReviewChapterForReviewer(chapterID, reviewerUserID int64) (*ReviewChapter, *Book, *FeedbackSubmission, error) {
	var chapter ReviewChapter
	var book Book
	var submission FeedbackSubmission
	var isbn, coverURL, description sql.NullString

	err := app.db.QueryRow(`
		SELECT
			rc.id, rc.draft_id, rc.label, rc.note_anchor_text, rc.note_body, rc.position, rc.created_at, rc.updated_at,
			rd.id, rd.public_id, rd.book_id, rd.reviewer_user_id, rd.created_at, rd.updated_at,
			b.id, b.public_id, b.owner_user_id, b.title, b.author_name, b.isbn, b.cover_url, b.description, b.status, b.created_at, b.updated_at
		FROM review_draft_chapters rc
		JOIN review_drafts rd ON rd.id = rc.draft_id
		JOIN books b ON b.id = rd.book_id
		WHERE rc.id = ? AND rd.reviewer_user_id = ?
	`, chapterID, reviewerUserID).Scan(
		&chapter.ID, &chapter.SubmissionID, &chapter.Label, &chapter.NoteAnchorText, &chapter.NoteBody, &chapter.Position, &chapter.CreatedAt, &chapter.UpdatedAt,
		&submission.ID, &submission.PublicID, &submission.BookID, &submission.ReviewerUserID, &submission.CreatedAt, &submission.UpdatedAt,
		&book.ID, &book.PublicID, &book.OwnerUserID, &book.Title, &book.AuthorName, &isbn, &coverURL, &description, &book.Status, &book.CreatedAt, &book.UpdatedAt,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	book.ISBN = isbn.String
	book.CoverURL = coverURL.String
	book.Description = description.String
	chapter.Pages, err = app.listReviewPages(chapter.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	return &chapter, &book, &submission, nil
}

func (app *application) getSubmittedReviewChapterForAuthor(chapterID, authorUserID int64) (*ReviewChapter, int64, error) {
	var chapter ReviewChapter
	var bookID int64
	err := app.db.QueryRow(`
		SELECT
			rc.id, rc.submission_id, rc.label, rc.note_anchor_text, rc.note_body, rc.position, rc.created_at, rc.updated_at,
			rc.author_reaction, rc.author_comment, rs.book_id
		FROM review_submission_chapters rc
		JOIN review_submissions rs ON rs.id = rc.submission_id
		JOIN books b ON b.id = rs.book_id
		WHERE rc.id = ? AND b.owner_user_id = ?
	`, chapterID, authorUserID).Scan(
		&chapter.ID, &chapter.SubmissionID, &chapter.Label, &chapter.NoteAnchorText, &chapter.NoteBody, &chapter.Position, &chapter.CreatedAt, &chapter.UpdatedAt,
		&chapter.AuthorReaction, &chapter.AuthorComment, &bookID,
	)
	if err != nil {
		return nil, 0, err
	}
	return &chapter, bookID, nil
}

func (app *application) getSubmittedReviewPageForAuthor(pageID, authorUserID int64) (*ReviewPage, int64, error) {
	var page ReviewPage
	var bookID int64
	err := app.db.QueryRow(`
		SELECT
			rp.id, rp.chapter_id, rp.page_number, rp.anchor_text, rp.comment_body, rp.position, rp.created_at, rp.updated_at,
			rp.author_reaction, rp.author_comment, rs.book_id
		FROM review_submission_pages rp
		JOIN review_submission_chapters rc ON rc.id = rp.chapter_id
		JOIN review_submissions rs ON rs.id = rc.submission_id
		JOIN books b ON b.id = rs.book_id
		WHERE rp.id = ? AND b.owner_user_id = ?
	`, pageID, authorUserID).Scan(
		&page.ID, &page.ChapterID, &page.PageNumber, &page.AnchorText, &page.CommentBody, &page.Position, &page.CreatedAt, &page.UpdatedAt,
		&page.AuthorReaction, &page.AuthorComment, &bookID,
	)
	if err != nil {
		return nil, 0, err
	}
	return &page, bookID, nil
}

func (app *application) updateReviewChapterResponse(chapterID int64, reaction, comment string) error {
	_, err := app.db.Exec(`
		UPDATE review_submission_chapters
		SET author_reaction = ?, author_comment = ?, updated_at = ?
		WHERE id = ?
	`, nullIfEmpty(reaction), nullIfEmpty(comment), time.Now().UTC(), chapterID)
	return err
}

func (app *application) updateReviewPageResponse(pageID int64, reaction, comment string) error {
	_, err := app.db.Exec(`
		UPDATE review_submission_pages
		SET author_reaction = ?, author_comment = ?, updated_at = ?
		WHERE id = ?
	`, nullIfEmpty(reaction), nullIfEmpty(comment), time.Now().UTC(), pageID)
	return err
}

func (app *application) submitFeedbackSubmission(submissionID int64) error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var draft FeedbackSubmission
	err = tx.QueryRow(`
		SELECT id, public_id, book_id, reviewer_user_id, created_at, updated_at
		FROM review_drafts
		WHERE id = ?
	`, submissionID).Scan(
		&draft.ID,
		&draft.PublicID,
		&draft.BookID,
		&draft.ReviewerUserID,
		&draft.CreatedAt,
		&draft.UpdatedAt,
	)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	submissionPublicID, err := app.generateUniquePublicIDTx(tx, "review_submissions", "public_id", "rs_")
	if err != nil {
		return err
	}
	res, err := tx.Exec(`
		INSERT INTO review_submissions (public_id, book_id, reviewer_user_id, submitted_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, submissionPublicID, draft.BookID, draft.ReviewerUserID, now, now)
	if err != nil {
		return err
	}
	submittedID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	rows, err := tx.Query(`
		SELECT id, label, note_anchor_text, note_body, position, created_at, updated_at
		FROM review_draft_chapters
		WHERE draft_id = ?
		ORDER BY position ASC, id ASC
	`, draft.ID)
	if err != nil {
		return err
	}

	var chapters []ReviewChapter
	for rows.Next() {
		var chapter ReviewChapter
		if err := rows.Scan(
			&chapter.ID,
			&chapter.Label,
			&chapter.NoteAnchorText,
			&chapter.NoteBody,
			&chapter.Position,
			&chapter.CreatedAt,
			&chapter.UpdatedAt,
		); err != nil {
			rows.Close()
			return err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, chapter := range chapters {
		chapterRes, err := tx.Exec(`
			INSERT INTO review_submission_chapters (
				submission_id, label, note_anchor_text, note_body, position,
				author_reaction, author_comment, created_at, updated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, submittedID, chapter.Label, chapter.NoteAnchorText, chapter.NoteBody, chapter.Position, nil, nil, chapter.CreatedAt, chapter.UpdatedAt)
		if err != nil {
			return err
		}
		newChapterID, err := chapterRes.LastInsertId()
		if err != nil {
			return err
		}

		pageRows, err := tx.Query(`
			SELECT page_number, anchor_text, comment_body, position, created_at, updated_at
			FROM review_draft_pages
			WHERE chapter_id = ?
			ORDER BY page_number ASC, position ASC, id ASC
		`, chapter.ID)
		if err != nil {
			return err
		}

		var pages []ReviewPage
		for pageRows.Next() {
			var page ReviewPage
			if err := pageRows.Scan(
				&page.PageNumber,
				&page.AnchorText,
				&page.CommentBody,
				&page.Position,
				&page.CreatedAt,
				&page.UpdatedAt,
			); err != nil {
				pageRows.Close()
				return err
			}
			pages = append(pages, page)
		}
		if err := pageRows.Err(); err != nil {
			pageRows.Close()
			return err
		}
		pageRows.Close()

		for _, page := range pages {
			if _, err := tx.Exec(`
				INSERT INTO review_submission_pages (
					chapter_id, page_number, anchor_text, comment_body, position,
					author_reaction, author_comment, created_at, updated_at
				)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, newChapterID, page.PageNumber, page.AnchorText, page.CommentBody, page.Position, nil, nil, page.CreatedAt, page.UpdatedAt); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func (app *application) listSubmittedFeedbackForBook(bookID int64) ([]SubmittedFeedbackGroup, error) {
	rows, err := app.db.Query(`
		SELECT rs.id, rs.reviewer_user_id, rs.submitted_at, u.name, u.email
		FROM review_submissions rs
		JOIN users u ON u.id = rs.reviewer_user_id
		WHERE rs.book_id = ?
		  AND rs.id = (
			SELECT rs2.id
			FROM review_submissions rs2
			WHERE rs2.book_id = rs.book_id
			  AND rs2.reviewer_user_id = rs.reviewer_user_id
			ORDER BY rs2.submitted_at DESC, rs2.id DESC
			LIMIT 1
		  )
		ORDER BY rs.submitted_at DESC, rs.id DESC
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
		chapters, err := app.listSubmittedReviewChapters(groups[i].SubmissionID)
		if err != nil {
			return nil, err
		}
		groups[i].Chapters = chapters
	}

	return groups, nil
}

func (app *application) listSubmittedFeedbackForReviewer(bookID, reviewerUserID int64) ([]SubmittedFeedbackGroup, error) {
	rows, err := app.db.Query(`
		SELECT rs.id, rs.reviewer_user_id, rs.submitted_at, u.name, u.email
		FROM review_submissions rs
		JOIN users u ON u.id = rs.reviewer_user_id
		WHERE rs.book_id = ? AND rs.reviewer_user_id = ?
		  AND rs.id = (
			SELECT rs2.id
			FROM review_submissions rs2
			WHERE rs2.book_id = rs.book_id
			  AND rs2.reviewer_user_id = rs.reviewer_user_id
			ORDER BY rs2.submitted_at DESC, rs2.id DESC
			LIMIT 1
		  )
		ORDER BY rs.submitted_at DESC, rs.id DESC
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
		chapters, err := app.listSubmittedReviewChapters(groups[i].SubmissionID)
		if err != nil {
			return nil, err
		}
		groups[i].Chapters = chapters
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

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastHyphen := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "book"
	}
	return slug
}

func bookRouteKey(title, publicID string) string {
	return slugify(title) + "--" + publicID
}

func parseBookPublicID(segment string) string {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return ""
	}
	if idx := strings.LastIndex(segment, "--"); idx >= 0 && idx+2 < len(segment) {
		return strings.TrimSpace(segment[idx+2:])
	}
	return segment
}

func nullIfEmpty(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func parseInt64(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func selectActiveReviewChapter(chapters []ReviewChapter, requestedID int64) *ReviewChapter {
	if len(chapters) == 0 {
		return nil
	}
	if requestedID != 0 {
		for i := range chapters {
			if chapters[i].ID == requestedID {
				return &chapters[i]
			}
		}
	}
	return &chapters[0]
}

func nextSuggestedPageNumber(pages []ReviewPage) int {
	if len(pages) == 0 {
		return 1
	}

	maxPage := 0
	for _, page := range pages {
		if page.PageNumber > maxPage {
			maxPage = page.PageNumber
		}
	}
	if maxPage <= 0 {
		return 1
	}
	return maxPage + 1
}

func currentNullString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func parseNullableTime(value sql.NullString) sql.NullTime {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return sql.NullTime{}
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999+00:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value.String); err == nil {
			return sql.NullTime{Time: parsed, Valid: true}
		}
	}

	log.Printf("parseNullableTime: unsupported value=%q", value.String)
	return sql.NullTime{}
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

func randomPublicIDSuffix(length int) (string, error) {
	const alphabet = "23456789abcdefghjkmnpqrstuvwxyz"
	buf := make([]byte, length)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf), nil
}

func addColumnIfMissing(db *sql.DB, table, column, alterSQL string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var dataType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(alterSQL)
	return err
}

func backfillPublicIDs(db *sql.DB, table, column, prefix string) error {
	rows, err := db.Query(fmt.Sprintf(`SELECT id FROM %s WHERE %s IS NULL OR trim(%s) = ''`, table, column, column))
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		publicID, err := generateUniquePublicIDDB(db, table, column, prefix)
		if err != nil {
			return err
		}
		if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, table, column), publicID, id); err != nil {
			return err
		}
	}
	return nil
}

func generateUniquePublicIDDB(db *sql.DB, table, column, prefix string) (string, error) {
	for i := 0; i < 12; i++ {
		suffix, err := randomPublicIDSuffix(8)
		if err != nil {
			return "", err
		}
		publicID := prefix + suffix
		var exists bool
		query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s = ?)`, table, column)
		if err := db.QueryRow(query, publicID).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return publicID, nil
		}
	}
	return "", fmt.Errorf("could not generate unique public id for %s", table)
}

func (app *application) generateUniquePublicID(table, column, prefix string) (string, error) {
	return generateUniquePublicIDDB(app.db, table, column, prefix)
}

func (app *application) generateUniquePublicIDTx(tx *sql.Tx, table, column, prefix string) (string, error) {
	for i := 0; i < 12; i++ {
		suffix, err := randomPublicIDSuffix(8)
		if err != nil {
			return "", err
		}
		publicID := prefix + suffix
		var exists bool
		query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s = ?)`, table, column)
		if err := tx.QueryRow(query, publicID).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return publicID, nil
		}
	}
	return "", fmt.Errorf("could not generate unique public id for %s", table)
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
	if !googleLoginEnabled() {
		return nil
	}

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

func googleLoginEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("GOOGLE_LOGIN_ENABLED")))
	switch value {
	case "", "1", "true", "yes":
		return true
	default:
		return false
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
