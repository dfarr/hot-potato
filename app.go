package main

import (
    "os"
    "log"
    _ "fmt"
    "time"
    "net/url"
    "net/http"
    "database/sql"
    "encoding/json"
    "html/template"
    "github.com/dfarr/hot-potato/Godeps/_workspace/src/github.com/gorilla/mux"
    "github.com/dfarr/hot-potato/Godeps/_workspace/src/github.com/satori/go.uuid"
    _ "github.com/dfarr/hot-potato/Godeps/_workspace/src/github.com/lib/pq"
)

var db *sql.DB

var PORT = os.Getenv("PORT")
var SLACK_CLIENT = os.Getenv("SLACK_CLIENT")
var SLACK_SECRET = os.Getenv("SLACK_SECRET")
var DATABASE_URL = os.Getenv("DATABASE_URL")

///////////////////////////////////////////////////////
// Response structs
///////////////////////////////////////////////////////

type SlackAPI struct {
    OK           bool
    Error        string
    Access_token string
    Scope        string
    Team_name    string
    Team_id      string
    Bot          struct {
        Bot_user_id      string
        Bot_access_token string
    }
    Members []struct {
        Name     string
        Presence string
    }
}

type SlackMessage struct {
    Team  string
    Back  string
    Next  string
    Token string
}

///////////////////////////////////////////////////////
// Handler functions
///////////////////////////////////////////////////////

func RootHandler(w http.ResponseWriter, r *http.Request) {
    tmpl, _ := template.ParseFiles("index.html")
    tmpl.Execute(w, map[string]string{"Client": SLACK_CLIENT})
}

func AuthHandler(w http.ResponseWriter, r *http.Request) {

    args := SlackAPI{}

    code := r.URL.Query().Get("code")

    res, err := http.PostForm("https://slack.com/api/oauth.access", url.Values{
        "client_id":     {SLACK_CLIENT},
        "client_secret": {SLACK_SECRET},
        "code":          {code},
    })

    if err != nil {
        log.Panic(err)
    }

    defer res.Body.Close()

    err = json.NewDecoder(res.Body).Decode(&args)

    if err != nil {
        log.Panic(err)
    }

    if args.OK {
        db.Exec("insert into team values ($1, $2)", args.Team_id, args.Bot.Bot_access_token)
    }

    http.Redirect(w, r, "/", 301)
}

func HotPWrapper(fn func(http.ResponseWriter, *http.Request, SlackMessage)) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {

        r.ParseForm()

        a := SlackMessage{Team: r.FormValue("team_id"), Back: "@" + r.FormValue("user_name"), Next: r.FormValue("text")}

        s, _ := db.Prepare("select token from team where uuid=$1")

        s.QueryRow(a.Team).Scan(&a.Token)

        args := SlackAPI{}
        pres := false

        res, _ := http.PostForm("https://slack.com/api/users.list", url.Values{
            "token":    {a.Token},
            "presence": {"1"},
        })

        defer res.Body.Close()

        json.NewDecoder(res.Body).Decode(&args)

        if args.OK {
            for _, member := range args.Members {
                if "@"+member.Name == a.Next && member.Presence == "active" {
                    pres = true
                }
            }
        }

        if !args.OK || !pres {
            w.Write([]byte("You must pass the potato to someone who is online!"))
            return
        }

        fn(w, r, a)
    }
}

func HotPHandler(w http.ResponseWriter, r *http.Request, args SlackMessage) {

    var reply string

    game := uuid.NewV4().String()

    stmt, _ := db.Exec("insert into game(uuid, team, active) select $1, $2, true where not exists(select 1 from game where team=$2 and active=true)", game, args.Team)

    rows, _ := stmt.RowsAffected()

    if rows == 0 {
        reply = "The hot potato is already being passed around!"
    }

    if rows == 1 {

        pass := uuid.NewV4().String()

        SendMessage(args.Next, "Hot potato from "+args.Back+", pass it on! (hint: use `/pass-it-on`)", args.Token)

        db.Exec("insert into pass values ($1, $2, $3, $4, $5)", pass, game, args.Back, args.Next, time.Now())

        go CheckPotato(pass, game, args.Token)

        reply = "Hot potato passed to " + args.Next
    }

    w.Write([]byte(reply))
}

func PassHandler(w http.ResponseWriter, r *http.Request, args SlackMessage) {

    var game string
    var back string
    var next string
    var reply string

    stmt, _ := db.Prepare("select game.uuid, pass.back, pass.next from game, pass where game.uuid=pass.game and game.team=$1 and game.active=true order by pass.time desc limit 1")

    stmt.QueryRow(args.Team).Scan(&game, &back, &next)

    if next != args.Back {
        reply = "You do not have the potato!"
    }

    if next == args.Back {

        pass := uuid.NewV4().String()

        SendMessage(args.Next, "Hot potato from "+args.Back+", pass it on! (hint: use `/pass-it-on`)", args.Token)

        db.Exec("insert into pass values ($1, $2, $3, $4, $5)", pass, game, args.Back, args.Next, time.Now())

        go CheckPotato(pass, game, args.Token)

        rows, _ := db.Query("select distinct back from pass where game=$1 and back!=$2 and back!=$3", game, args.Back, args.Next)

        for rows.Next() {

            var send string

            rows.Scan(&send)

            SendMessage(send, args.Back+" passed the potato to "+args.Next+"!", args.Token)
        }

        reply = "Hot potato passed to " + args.Next
    }

    w.Write([]byte(reply))
}

func CheckPotato(pass string, game string, token string) {

    time.Sleep(60 * time.Second)

    var uuid string
    var back string
    var next string

    stmt, _ := db.Prepare("select uuid, back, next from pass where game=$1 order by time desc limit 1")

    stmt.QueryRow(game).Scan(&uuid, &back, &next)

    if uuid == pass {

        SendMessage(next, "You're on fire!", token)

        db.Exec("update game set active=false where uuid=$1", game)

        rows, _ := db.Query("select distinct back from pass where game=$1 and back!=$2", game, next)

        for rows.Next() {

            var send string

            rows.Scan(&send)

            SendMessage(send, next+" is on fire!", token)
        }
    }
}

func SendMessage(channel string, message string, token string) {

    res, _ := http.PostForm("https://slack.com/api/chat.postMessage", url.Values{
        "token":    {token},
        "channel":  {channel},
        "text":     {message},
        "username": {"HotPotato"},
        "icon_url": {"https://weedsuptomeknees.files.wordpress.com/2013/11/potato-bullet.jpg"},
    })

    defer res.Body.Close()
}

func main() {

    ///////////////////////////////////////////////////////
    // Initialize the db
    ///////////////////////////////////////////////////////

    var err1 error

    db, err1 = sql.Open("postgres", DATABASE_URL)

    if err1 != nil {
        log.Fatal(err1)
    }

    defer db.Close()

    tx, err2 := db.Begin()

    if err2 != nil {
        log.Fatal(err2)
    }

    tx.Exec("create table if not exists game (uuid text primary key, team text, active boolean)")
    tx.Exec("create table if not exists team (uuid text primary key, token text)")
    tx.Exec("create table if not exists pass (uuid text primary key, game text, back text, next text, time timestamp, foreign key (game) references game (uuid))")

    err3 := tx.Commit()

    if err3 != nil {
        log.Fatal(err3)
    }

    ///////////////////////////////////////////////////////
    // Set up routes
    ///////////////////////////////////////////////////////

    r := mux.NewRouter()

    r.HandleFunc("/", RootHandler)

    r.HandleFunc("/hotp", HotPWrapper(HotPHandler))

    r.HandleFunc("/pass", HotPWrapper(PassHandler))

    r.HandleFunc("/auth/slack/callback", AuthHandler)

    // bind to port
    http.ListenAndServe(":"+PORT, r)
}
