package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	redigo "github.com/garyburd/redigo/redis"
	log "github.com/Sirupsen/logrus"
	jason "github.com/antonholmquist/jason"
	jwt "github.com/dgrijalva/jwt-go"
	mux "github.com/gorilla/mux"
	sessions "github.com/gorilla/sessions"
	websocket "github.com/gorilla/websocket"
	alice "github.com/justinas/alice"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"sync"
	"os/signal"
	"syscall"
)

//TODO
//verify trade url if(url.match(/token=([\w-]+)/)) with regex
//prevent directory listing
//https://gyazo.com/440cd2eaae0ad7a48e84604d356d73c4
//implement routers http://www.gorillatoolkit.org/pkg/mux
//websocket read limit
//session cookie
//null byte trimming
//check content length and deny unreasonably large requests
//middleware
//rate limiting, throttler
//make sure jwt token is only one time use
//socket timeouts
//status codes for websocket
//prevent multiple connections from same ip
//set ulimit
//socket pinging so it doesnt auto close
//socket timeouts

//Make sure to use https port in HOST_ADDR
const HOST_ADDR string = "24.4.237.252:443"
const HOST_HTTP_PORT string = ":80"
const HOST_HTTPS_PORT string = ":443"

const TOKEN_VALID_TIME = 30
const SESS_VALID_TIME = 86400 * 3
const CLEANUP_DELAY = 5

var COOKIE_SECRET string
var STEAM_API_KEY string
var INDEX_HTML string
var HOME_HTML string
var NOT_FOUND_HTML string

var redis redigo.Conn
var redisChan chan *RedisToken = make(chan *RedisToken, 100)
var broadcastChan chan *Broadcast = make(chan *Broadcast, 100)
var steamApiUrl string = "https://api.steampowered.com/ISteamUser/GetPlayerSummaries/v0002/?"
var sessionStore *sessions.CookieStore
var upgrader = websocket.Upgrader{
	HandshakeTimeout: time.Second * 10,
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
}

type WebsocketMessage struct {
	MsgType int
	Msg *jason.Object
	Code int
	ReadError error
}

type RedisToken struct {
	Code int
	Token string
	Sid string
	Callback chan int
}

type SocketConn struct {
	Sid string
	Conn *websocket.Conn
	Callback chan int
	ConnAlive bool
	Sync *sync.Mutex
	KeepInDb bool
}

type Broadcast struct {
	Msg map[string]string
	Conn *SocketConn
	Code int
	Callback chan *SocketConn
	//0 add to array of active clients
	//1 perform cleanup operation
	//2 disable client with specified steamid
	//3 broadcast chat message to all clients
}

func MainHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-type", "text/html")
	fmt.Fprint(w, INDEX_HTML)
}

func HomeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-type", "text/html")

	session, sessionErr := sessionStore.Get(r, "session")
	if sessionErr != nil {
		log.Error("Error getting session for ", r.RemoteAddr, ": ", sessionErr.Error())
		removeSessionCookie(session, w, r)
	}

	if !session.IsNew {
		expTime, _ := strconv.ParseInt(session.Values["exp"].(string), 10, 64)
		remAddr, _ := session.Values["ip"].(string)
		isExpired := expTime <= time.Now().Unix()
		isDifferentIp := strings.Compare(remAddr, strings.Split(r.RemoteAddr, ":")[0]) != 0
		//TODO make code better
		if isExpired || isDifferentIp {
			if isDifferentIp {
				log.Warn("Session ip addr mismatch: ", r.RemoteAddr, ", ", remAddr)
			} else if isExpired {
				log.Warn("Expired session from ", r.RemoteAddr)
			}
			removeSessionCookie(session, w, r)
			http.Redirect(w, r, "https://"+HOST_ADDR+"/oid/login", http.StatusMovedPermanently)
			return
		}

		log.Info("Logged in with session for ", r.RemoteAddr)

		if genSockAuthCookie(w, r, session.Values["sid"].(string)) != nil {
			http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
			return
		}
	}
	fmt.Fprint(w, HOME_HTML)
}

func marshalAndSend(data map[string]string, socketConn *SocketConn, needLock bool) error {
	json, jsonErr := json.Marshal(data)
	if jsonErr != nil {
		log.Error("Json marshal error for ", socketConn.Conn.RemoteAddr().String(), ": ", jsonErr.Error())
		return jsonErr
	}

	var sendErr error
	if needLock {
		socketConn.Sync.Lock()
	}
	if socketConn.ConnAlive {
		//TODO catch error
		socketConn.Conn.WriteMessage(1, json)
	} else {
		return nil
	}
	if needLock {
		socketConn.Sync.Unlock()
	}

	if sendErr != nil {
		//TODO add retry mechanism
		log.Error("Error sending message to ", socketConn.Conn.RemoteAddr().String(), ": ", sendErr.Error())
		if needLock {
			socketConn.Sync.Lock()
		}
		socketConn.ConnAlive = false
		if needLock {
			socketConn.Sync.Unlock()
		}
		return sendErr
	}
	return nil
}

func SockHandler(w http.ResponseWriter, r *http.Request) {

	if !websocket.IsWebSocketUpgrade(r) {
		log.Warn("Invalid request to /sock from ", r.RemoteAddr, ", redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}

	conn, connErr := upgrader.Upgrade(w, r, nil)
	if connErr != nil {
		log.Error("Websocket upgrade error for ", r.RemoteAddr, ": ", connErr.Error())
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}
	log.Info("Websocket connected from ", conn.RemoteAddr().String())

	socketConn := &SocketConn{
		Conn: conn,
		ConnAlive : true,
		Sync : new(sync.Mutex),
	}

	conn.SetReadLimit(2048)
	_, data, readErr := conn.ReadMessage()
	if readErr != nil || len(data) < 1 || strings.Contains(string(data), "=") == false {
		if readErr != nil {
			log.Error("Socket read (auth token) error from, ", conn.RemoteAddr().String(), ": ", readErr.Error())
		} else {
			log.Warn("Invalid token received from ", conn.RemoteAddr().String())
		}
		conn.Close()
		return
	}

	cookieStr := strings.Split(string(bytes.Trim(data, "\x00")), "=")[1]

	token, tokenErr := jwt.Parse(cookieStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Invalid signing method: ", token.Header["alg"])
		}
		return []byte(COOKIE_SECRET), nil
	})

	if tokenErr != nil {
		log.Error("Error validating token from, ", conn.RemoteAddr().String(), ": ", tokenErr.Error())
		marshalAndSend(map[string]string{"is_valid": "false", "code":"0"}, socketConn, true)
		conn.Close()
		return
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		log.Error("Error asserting types or invalid token from ", conn.RemoteAddr().String())
		marshalAndSend(map[string]string{"is_valid": "false", "code":"0"}, socketConn, true)
		conn.Close()
		return
	}

	expTime, _ := strconv.ParseInt(claims["exp"].(string), 10, 64)
	remAddr, _ := claims["ip"].(string)
	steam64id, _ := claims["sid"].(string)

	isExpired := expTime <= time.Now().Unix()
	isDifferentIp := strings.Compare(remAddr, strings.Split(conn.RemoteAddr().String(), ":")[0]) != 0
	if isExpired || isDifferentIp {
		if isExpired {
			log.Warn("Expired token from ", conn.RemoteAddr().String())
		} else if isDifferentIp {
			log.Warn("Token ip addr mismatch: ", conn.RemoteAddr().String(), ", ", remAddr)
		}
		marshalAndSend(map[string]string{"is_valid": "false", "code":"0"}, socketConn, true)
		conn.Close()
		return
	}

	callbackChan := make(chan int)
	redisChan <- &RedisToken{
		Code : 0,
		Token : cookieStr,
		Sid : steam64id,
		Callback : callbackChan,
	}

	if <-callbackChan == 1 {
		log.Warn("Token from ", conn.RemoteAddr().String(), " has already been used")
		marshalAndSend(map[string]string{"is_valid": "false", "code":"0"}, socketConn, true)
		conn.Close()
		return
	}

	if marshalAndSend(map[string]string{"is_valid": "true", "code":"0"}, socketConn, true) != nil {
		conn.Close()
		return
	}

	log.Info("Token validated for ", conn.RemoteAddr().String())

	socketConn.Sid = steam64id
	socketConn.Callback = callbackChan
	socketConn.KeepInDb = false

	params := url.Values{}
	params.Add("key", STEAM_API_KEY)
	params.Add("steamids", steam64id)
	resp, respErr := http.Get(steamApiUrl + params.Encode())
	if readErr != nil {
		log.Error("Error fetching userinfo with steam api ", conn.RemoteAddr().String(), ": ", respErr.Error())
		marshalAndSend(map[string]string{"code":"4"}, socketConn, true)
		conn.Close()
		return
	}

	apiData, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.Error("Error reading response from steamapi for ", conn.RemoteAddr().String(), ": ", readErr.Error())
		marshalAndSend(map[string]string{"code":"4"}, socketConn, true)
		conn.Close()
		return
	}

	payload, _ := jason.NewObjectFromBytes(apiData)
	allUserData, _ := payload.GetObjectArray("response", "players")
	for _, key := range allUserData {
		communityState, _ := key.GetInt64("communityvisibilitystate")
		profileState, _ := key.GetInt64("profilestate")
		if communityState == 3 && profileState == 1 {
			userNickname, _ := key.GetString("personaname")
			userAvatar, _ := key.GetString("avatarfull")
			userInfo := map[string]string{"nickname": userNickname, "avatar": userAvatar, "code": "1"}
			if marshalAndSend(userInfo, socketConn, true) != nil {
				log.Error("Error sending userinfo to ", conn.RemoteAddr().String())
				conn.Close()
				return
			}
		} else {
			log.Warn(conn.RemoteAddr().String(), " ", steam64id, " steam profile is private or not setup")
			marshalAndSend(map[string]string{"code":"6"}, socketConn, true)
			conn.Close()
			return
		}
	}

	//Add connection to broadcast loop
	broadcastChan <- &Broadcast{
		Conn : socketConn,
		Code : 0,
	}

	//status 0 = ok
	//status 1 = quit
	defer fmt.Println("main exit")
	msgChan := make(chan *WebsocketMessage)
	go socketReadLoop(socketConn, msgChan)
	for {
		select {
		case state := <-socketConn.Callback:
				//reasons
				//0 keep in redis
				//1 remove from redis

				//0 token validated
				//1 token invalid
				//2 another user signed in as
				//3 too many errors
				if state == 2 {
					log.Warn("Another user signed in as ", conn.RemoteAddr().String())
					marshalAndSend(map[string]string{"code": "2"}, socketConn, false)
					socketConn.ConnAlive = false
					socketConn.KeepInDb = true
					conn.Close()
					socketConn.Callback <- 2
					return
				} else if state == 3 {
					log.Warn("Too many errors for ", conn.RemoteAddr().String())
					marshalAndSend(map[string]string{"code": "5"}, socketConn, false)
					socketConn.ConnAlive = false
					conn.Close()
					socketConn.Callback <- 3
					return
				}
			case data := <-msgChan:
				if data.ReadError != nil || data.Code == -1 {
					socketConn.Sync.Lock()
					socketConn.ConnAlive = false
					socketConn.Sync.Unlock()
					conn.Close()
					return
				} else {
					fmt.Println(data.Msg)
				}
		}
	}
}

//Add max number of messages read
//something like 60 messages per min
func socketReadLoop(socketConn *SocketConn, msgChan chan *WebsocketMessage) {
	defer fmt.Println("Readloop exited")
	remoteAddr := socketConn.Conn.RemoteAddr().String()
	errCount := 0
	for {
		if errCount > 3 {
			socketConn.Sync.Lock()
			if socketConn.ConnAlive {
				socketConn.Callback <- 3
				if <-socketConn.Callback == 3 {
					socketConn.Sync.Unlock()
					return
				}
			} else {
				socketConn.Sync.Unlock()
				return
			}
		}

		mType, data, err := socketConn.Conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, 1001) == true {
				log.Info("Client ", remoteAddr, " went away")
			} else if !socketConn.ConnAlive {
				log.Warn(remoteAddr, " socket connection forcibly closed")
			} else {
				log.Error("Read message error for ", remoteAddr, ": ", err.Error())
			}
			if !socketConn.KeepInDb {
				redisChan <- &RedisToken{
					Code : 1,
					Sid : socketConn.Sid,
				}
			}
			if socketConn.ConnAlive {
				msgChan <- &WebsocketMessage{
					Code : -1,
				}
			}
			return
		}

		payload, parseErr := jason.NewObjectFromBytes(data)
		if parseErr != nil {
			log.Error("Message parse error for ", remoteAddr, ": ", parseErr.Error())
			errCount++
			marshalAndSend(map[string]string{"code":"4"}, socketConn, true)
			continue

		}

		msgCode, msgCodeErr := payload.GetString("code")
		if msgCodeErr != nil {
			log.Warn("No field code in received message ", remoteAddr)
			errCount++
			marshalAndSend(map[string]string{"code":"4"}, socketConn, true)
			continue
		}

		code, convErr := strconv.Atoi(msgCode)
		if convErr != nil {
			log.Error("Error converting code to int for ", remoteAddr, ": ", convErr.Error())
			errCount++
			marshalAndSend(map[string]string{"code":"4"}, socketConn, true)
			continue
		}

		msgChan <- &WebsocketMessage{
			MsgType : mType,
			Msg : payload,
			Code : code,
			ReadError : err,
		}
	}
}

func redisLoop(rChan chan *RedisToken) {
	callback := make(chan *SocketConn)
	for {
		input := <-rChan
		//db 0 for tokens 1 for steamids
		//code 0 add sid, 1 remove sid
		if input.Code == 0 {
			if _, err := redis.Do("SELECT", "0"); err != nil {
				log.Error("Error changing redis database: ", err.Error())
			}
			//Check if token has only been used once
			tVal, redisTErr := redis.Do("SET", input.Token, input.Sid, "NX", "EX", strconv.Itoa(TOKEN_VALID_TIME))
			if redisTErr != nil {
				log.Error("Error setting token in redis: ", redisTErr.Error())
				input.Callback <- 1
				continue
			}
			//If new token, check if another user is logged in already with same sid
			if tVal != nil {
				if _, err := redis.Do("SELECT", "1"); err != nil {
					log.Error("Error changing redis database: ", err.Error())
				}
				gVal, _ := redis.Do("SET", "online."+input.Sid, input.Sid, "NX")
				//if no duplicate sid is found send 0 to callback asking socket to proceed
				if gVal != nil {
					input.Callback <- 0
					continue
				} else {
					//else check if sid is active in broadcast loop
					broadcastChan <- &Broadcast{
						Code : 2,
						Conn : &SocketConn{
							Sid : input.Sid,
						},
						Callback : callback,
					}
					closeClient := <-callback
					//if client is active, close its connection and proceed with current socket
					closeClient.Sync.Lock()
					if closeClient.ConnAlive {
						closeClient.Callback <- 2
						if <-closeClient.Callback == 2 {
							input.Callback <- 0
						}
					} else {
						input.Callback <- 0
					}
					closeClient.Sync.Unlock()
				}
			} else {
				input.Callback <- 1
			}
		} else if input.Code == 1 {
			if _, err := redis.Do("SELECT", "1"); err != nil {
				log.Error("Error changing redis database: ", err.Error())
				continue
			}
			redis.Do("DEL", "online."+input.Sid)
			log.Info("Removed ", input.Sid, " from redis")
		}
	}
}

//TODO add recover in all functions that arent handlers
func broadcastLoop(broadcastChan chan *Broadcast) {
	activeConns := make([]*SocketConn, 0)
	for {
		input := <-broadcastChan
		if input.Code == 0 {
			activeConns = append(activeConns, input.Conn)
		} else if input.Code == 1 {
			tempConns := make([]*SocketConn, 0)
			for _, key := range activeConns {
				key.Sync.Lock()
				if key.ConnAlive {
					tempConns = append(tempConns, key)
				}
				key.Sync.Unlock()
			}
			activeConns = tempConns
		} else if input.Code == 2 {
			found := false
			for _, key := range activeConns {
				if key.Sid == input.Conn.Sid {
					input.Callback <- key
					found = true
					break
				}
			}
			if found {
				continue
			} else {
				input.Callback <- &SocketConn{
					//Doesnt exist
					ConnAlive : false,
				}
			}
		} else if input.Code == 3 {
			for _, key := range activeConns {
				if key.ConnAlive {
					go marshalAndSend(map[string]string{"kek":"kek"}, key, true)
				}
			}
		}
		fmt.Println(activeConns)
	}
}

func broadcastCleanup(broadcastChan chan *Broadcast) {
	for {
		time.Sleep(time.Second * CLEANUP_DELAY)
		broadcastChan <- &Broadcast{
			Code : 1,
		}
	}
}

func OidHandler(w http.ResponseWriter, r *http.Request) {
	mode := trimNullBytes(mux.Vars(r)["mode"])
	if mode == "login" {
		OidLoginHandler(w, r, false)
	} else if mode == "login_s" {
		OidLoginHandler(w, r, true)
	} else if mode == "auth" {
		OidAuthHandler(w, r, false)
	} else if mode == "auth_s" {
		OidAuthHandler(w, r, true)
	} else if mode == "logout" {
		OidLogoutHandler(w, r)
	} else {
		log.Warn("Invalid oid mode from ", r.RemoteAddr, ", redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}
}

func OidLogoutHandler(w http.ResponseWriter, r *http.Request) {
	session, sessionErr := sessionStore.Get(r, "session")
	if sessionErr != nil {
		log.Error("Error getting session for ", r.RemoteAddr, ": ", sessionErr.Error())
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
	}
	if !session.IsNew {
		removeSessionCookie(session, w, r)
	}

	log.Info("Logout sequence finished for ", r.RemoteAddr, " redirecting to /")
	http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
}

func OidLoginHandler(w http.ResponseWriter, r *http.Request, saveSession bool) {
	params := url.Values{}
	params.Add("openid.ns", "http://specs.openid.net/auth/2.0")
	params.Add("openid.mode", "checkid_setup")
	if saveSession {
		params.Add("openid.return_to", "https://"+HOST_ADDR+"/oid/auth_s")
	} else {
		params.Add("openid.return_to", "https://"+HOST_ADDR+"/oid/auth")
	}
	params.Add("openid.realm", "https://"+HOST_ADDR)
	params.Add("openid.identity", "http://specs.openid.net/auth/2.0/identifier_select")
	params.Add("openid.claimed_id", "http://specs.openid.net/auth/2.0/identifier_select")

	loginUrl := "https://steamcommunity.com/openid/login?" + params.Encode()
	http.Redirect(w, r, loginUrl, http.StatusMovedPermanently)
}

func OidAuthHandler(w http.ResponseWriter, r *http.Request, saveSession bool) {
	r.ParseForm()
	params := url.Values{}
	params.Add("openid.assoc_handle", r.Form.Get("openid.assoc_handle"))
	params.Add("openid.signed", r.Form.Get("openid.signed"))
	params.Add("openid.sig", r.Form.Get("openid.sig"))
	params.Add("openid.ns", "http://specs.openid.net/auth/2.0")
	params.Add("openid.mode", "check_authentication")
	params.Add("openid.op_endpoint", r.Form.Get("openid.op_endpoint"))
	params.Add("openid.claimed_id", r.Form.Get("openid.claimed_id"))
	params.Add("openid.identity", r.Form.Get("openid.identity"))
	params.Add("openid.return_to", r.Form.Get("openid.return_to"))
	params.Add("openid.response_nonce", r.Form.Get("openid.response_nonce"))

	log.Info("Authenticating login request from ", r.RemoteAddr, " with Steam...")

	var steam64id string
	if len(params.Get("openid.identity")) == 53 {
		steam64id = trimNullBytes(params.Get("openid.identity"))[36:53]
		match, regErr := regexp.MatchString("[0-9]", steam64id)
		if match == false {
			log.Warn("Invalid (non-numeric) steam64 ID returned for ", r.RemoteAddr, ", redirecting to /")
			http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
			return
		} else if regErr != nil {
			log.Error("Regex error on steam64 ID for ", r.RemoteAddr, ", redirecting to /")
			http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
			return
		}
	} else {
		log.Warn("Invalid (invalid length) steam64 ID returned for ", r.RemoteAddr, ", redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}

	resp, err := http.PostForm("https://steamcommunity.com/openid/login", params)
	if err != nil {
		log.Error("Auth request for ", r.RemoteAddr, " failed, redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("Read auth response for ", r.RemoteAddr, " failed, redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}

	is_valid := strings.Split(strings.Split(strings.Trim(string(data), "\n"), "\n")[1], ":")[1]
	if strings.Compare(is_valid, "true") == 0 {
		log.Info("Addr ", r.RemoteAddr, " has been authenticated")

		if saveSession {
			session, sessionErr := sessionStore.Get(r, "session")
			if sessionErr != nil {
				log.Error("Error getting session for ", r.RemoteAddr, ": ", sessionErr.Error())
			}

			session.Options = &sessions.Options{
				Path:     "/",
				HttpOnly: true,
				MaxAge:   SESS_VALID_TIME,
				Secure:   true,
			}

			session.Values["sid"] = steam64id
			session.Values["exp"] = strconv.FormatInt(time.Now().Unix()+SESS_VALID_TIME, 10)
			session.Values["ip"] = strings.Split(r.RemoteAddr, ":")[0]

			if err := session.Save(r, w); err != nil {
				log.Error("Error saving session for ", r.RemoteAddr, ": ", err.Error(), " redirecting to /")
				http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
				return
			}

			log.Info("Generated session cookie for ", r.RemoteAddr)
		} else {
			if genSockAuthCookie(w, r, steam64id) != nil {
				http.Redirect(w, r, "https://"+HOST_ADDR+"/", http.StatusMovedPermanently)
				return
			}
		}

		log.Info("Redirecting ", r.RemoteAddr, " to /home")
		http.Redirect(w, r, "https://"+HOST_ADDR+"/home", http.StatusMovedPermanently)
		return
	} else {
		log.Warn("Addr ", r.RemoteAddr, " auth fail, redirecting to /")
		http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
		return
	}
}

func removeSessionCookie(session *sessions.Session, w http.ResponseWriter, r *http.Request) {
	session.Options = &sessions.Options{
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Secure:   true,
	}
	if err := session.Save(r, w); err != nil {
		log.Error("Error removing session for ", r.RemoteAddr, ": ", err.Error())
		return
	}
	log.Info("Requested removal of session for ", r.RemoteAddr)
}

func genSockAuthCookie(w http.ResponseWriter, r *http.Request, steam64id string) error {
	tokenExp := time.Now().Add(time.Second * TOKEN_VALID_TIME)

	//TODO add mode field "websocket"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sid": steam64id,
		"exp": strconv.FormatInt(tokenExp.Unix(), 10),
		"ip":  strings.Split(string(r.RemoteAddr), ":")[0],
	})

	tokenString, tokenErr := token.SignedString([]byte(COOKIE_SECRET))
	if tokenErr != nil {
		log.Error("Error generating token for ", r.RemoteAddr, ": ", tokenErr.Error())
		return tokenErr
	}

	cookie := &http.Cookie{
		Name:     "sock_auth",
		Value:    tokenString,
		Expires:  tokenExp,
		HttpOnly: false,
		Secure:   true,
		Path:     "/home",
	}
	http.SetCookie(w, cookie)
	log.Info("Set jwt sock_auth cookie for ", r.RemoteAddr)

	return nil
}

func trimNullBytes(input string) string {
	return string(bytes.Trim([]byte(input), "\x00"))
}

func RedirectToHttps(w http.ResponseWriter, r *http.Request) {
	log.Info("Redirecting ", r.RemoteAddr, " to HTTPS /")
	http.Redirect(w, r, "https://"+HOST_ADDR, http.StatusMovedPermanently)
}

func NoDirListing(h http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func NotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-type", "text/html")
	fmt.Fprint(w, NOT_FOUND_HTML)
}

func RecoverHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Error("Unexpected panic from ", r.RemoteAddr, ": ", err)
			}
		}()
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func LogHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		log.Info(r.RemoteAddr, " ", r.Method, " ", r.URL.Path)
		startTime := time.Now()
		next.ServeHTTP(w, r)
		log.Info(r.RemoteAddr, " ", r.Method, " ", r.URL.Path, " completed in ", time.Now().Sub(startTime))
	}
	return http.HandlerFunc(fn)
}

func cleanup() {
	if _, err := redis.Do("SELECT", "0"); err != nil {
		log.Error("Error changing redis database: ", err.Error())
	}
	redis.Do("FLUSHALL")
	if _, err := redis.Do("SELECT", "1"); err != nil {
		log.Error("Error changing redis database: ", err.Error())
	}
	redis.Do("FLUSHALL")
}

func main() {
	log.SetLevel(log.InfoLevel)
	log.SetOutput(os.Stdout)

	apiKey, apiKeyFileError := ioutil.ReadFile("secure/apikey.txt")
	if apiKeyFileError != nil {
		log.Fatal("Error loading API key:", apiKeyFileError.Error())
		return
	}
	STEAM_API_KEY = strings.Trim(string(apiKey), "\n ")
	log.Info("Loaded API key")

	cookieSecret, cookieSecretError := ioutil.ReadFile("secure/cookie_secret.txt")
	if cookieSecretError != nil {
		log.Fatal("Error loading cookie secret: ", cookieSecretError.Error())
		return
	}
	COOKIE_SECRET = strings.Trim(string(cookieSecret), "\n ")
	log.Info("Loaded jwt cookie secret")

	sessionSecret, sessionSecretError := ioutil.ReadFile("secure/session_secret.txt")
	if sessionSecretError != nil {
		log.Fatal("Error loading session secret: ", sessionSecretError.Error())
		return
	}
	sessionStore = sessions.NewCookieStore(sessionSecret)
	sessionStore.MaxAge(SESS_VALID_TIME)
	log.Info("Loaded session store")

	indexHtmlFile, indexHtmlFileError := ioutil.ReadFile("index.html")
	if indexHtmlFileError != nil {
		log.Fatal("Error loading index.html: ", indexHtmlFileError.Error())
		return
	}
	INDEX_HTML = strings.Trim(string(indexHtmlFile), "\n ")
	log.Info("Loaded index.html")

	homeHtmlFile, homeHtmlFileError := ioutil.ReadFile("home.html")
	if homeHtmlFileError != nil {
		log.Fatal("Error loading index.html: ", homeHtmlFileError.Error())
		return
	}
	HOME_HTML = strings.Trim(string(homeHtmlFile), "\n ")
	log.Info("Loaded home.html")

	notFoundFile, notFoundFileError := ioutil.ReadFile("404.html")
	if notFoundFileError != nil {
		log.Fatal("Error loading 404.html: ", notFoundFileError.Error())
		return
	}
	NOT_FOUND_HTML = strings.Trim(string(notFoundFile), "\n ")
	log.Info("Loaded 404.html")

	redisKey, redisKeyError := ioutil.ReadFile("secure/redis_key.txt")
	if redisKeyError != nil {
		log.Fatal("Error loading redis password: ", notFoundFileError.Error())
		return
	}
	redisConn, connErr := redigo.Dial("tcp", ":6379")
	if connErr != nil {
		log.Fatal("Error connecting to redis: ", connErr.Error())
		return
	}
	redis = redisConn
	if _, err := redis.Do("AUTH", strings.Trim(string(redisKey), "\n")); err != nil {
		log.Fatal("Error authenticating with redis: ", err.Error())
	}
	go redisLoop(redisChan)
	cleanup()
	log.Info("Started redis")

	go broadcastLoop(broadcastChan)
	go broadcastCleanup(broadcastChan)
	log.Info("Started broadcast loop")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
    go func() {
        <-c
        cleanup()
        os.Exit(1)
    }()

	r := mux.NewRouter()
	r.StrictSlash(true)
	r.NotFoundHandler = http.HandlerFunc(NotFound)
	chain := alice.New(RecoverHandler, LogHandler)

	r.Handle("/", chain.ThenFunc(MainHandler)).Methods("GET")
	r.Handle("/home", chain.ThenFunc(HomeHandler)).Methods("GET")
	r.Handle("/sock", chain.ThenFunc(SockHandler)).Methods("GET")
	r.Handle("/oid/{mode:[a-z_]+}", chain.ThenFunc(OidHandler)).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", NoDirListing(http.FileServer(http.Dir("./static/")))))

	http.Handle("/", r)

	log.Info("Starting servers...")

	//TODO catch error
	go http.ListenAndServeTLS(HOST_HTTPS_PORT, "secure/server.crt", "secure/server.key", nil)
	http.ListenAndServe(HOST_HTTP_PORT, http.HandlerFunc(RedirectToHttps))
}
