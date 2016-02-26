/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk/encryptor"
	"github.com/gravitational/teleport/lib/events/boltlog"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/recorder/boltrec"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/services"
	sess "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gokyle/hotp"
	"github.com/gravitational/roundtrip"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	. "gopkg.in/check.v1"
)

func TestWeb(t *testing.T) { TestingT(t) }

type WebSuite struct {
	node        *srv.Server
	srvAddress  string
	srvHostPort string
	bk          *encryptedbk.ReplicatedBackend
	roleAuth    *auth.AuthWithRoles
	dir         string
	user        string
	domainName  string
	signer      ssh.Signer
	tunServer   *auth.TunServer
	webServer   *httptest.Server
	freePorts   []string
}

var _ = Suite(&WebSuite{})

func (s *WebSuite) SetUpSuite(c *C) {
	utils.InitLoggerDebug()
}

func (s *WebSuite) SetUpTest(c *C) {
	s.dir = c.MkDir()

	u, err := user.Current()
	c.Assert(err, IsNil)
	s.user = u.Username

	s.freePorts, err = utils.GetFreeTCPPorts(3)
	c.Assert(err, IsNil)

	baseBk, err := boltbk.New(filepath.Join(s.dir, "db"))
	c.Assert(err, IsNil)
	s.bk, err = encryptedbk.NewReplicatedBackend(baseBk,
		filepath.Join(s.dir, "keys"), nil,
		encryptor.GetTestKey)
	c.Assert(err, IsNil)

	s.domainName = "localhost"
	authServer := auth.NewAuthServer(s.bk, authority.New(), s.domainName)

	eventsLog, err := boltlog.New(filepath.Join(s.dir, "boltlog"))
	c.Assert(err, IsNil)

	c.Assert(authServer.UpsertCertAuthority(
		*services.NewTestCA(services.UserCA, s.domainName), backend.Forever), IsNil)
	c.Assert(authServer.UpsertCertAuthority(
		*services.NewTestCA(services.HostCA, s.domainName), backend.Forever), IsNil)

	recorder, err := boltrec.New(s.dir)
	c.Assert(err, IsNil)

	sessionServer := sess.New(baseBk)
	s.roleAuth = auth.NewAuthWithRoles(authServer,
		auth.NewStandardPermissions(),
		eventsLog,
		sessionServer,
		teleport.RoleAdmin,
		recorder)

	// set up host private key and certificate
	hpriv, hpub, err := authServer.GenerateKeyPair("")
	c.Assert(err, IsNil)
	hcert, err := authServer.GenerateHostCert(
		hpub, s.domainName, s.domainName, teleport.RoleAdmin, 0)
	c.Assert(err, IsNil)

	// set up user CA and set up a user that has access to the server
	s.signer, err = sshutils.NewSigner(hpriv, hcert)
	c.Assert(err, IsNil)

	limiter, err := limiter.NewLimiter(
		limiter.LimiterConfig{
			MaxConnections: 100,
			Rates: []limiter.Rate{
				limiter.Rate{
					Period:  1 * time.Second,
					Average: 100,
					Burst:   400,
				},
				limiter.Rate{
					Period:  40 * time.Millisecond,
					Average: 1000,
					Burst:   4000,
				},
			},
		},
	)
	c.Assert(err, IsNil)

	// start node
	nodePort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]

	s.srvAddress = fmt.Sprintf("127.0.0.1:%v", nodePort)
	node, err := srv.New(
		utils.NetAddr{AddrNetwork: "tcp", Addr: s.srvAddress},
		s.domainName,
		[]ssh.Signer{s.signer},
		s.roleAuth,
		limiter,
		s.dir,
		srv.SetShell("/bin/sh"),
		srv.SetSessionServer(sessionServer),
		srv.SetRecorder(recorder),
	)
	c.Assert(err, IsNil)
	s.node = node

	c.Assert(s.node.Start(), IsNil)

	revTunServer, err := reversetunnel.NewServer(
		utils.NetAddr{
			AddrNetwork: "tcp",
			Addr:        fmt.Sprintf("%v:0", s.domainName),
		},
		[]ssh.Signer{s.signer},
		s.roleAuth, limiter,
		reversetunnel.ServerTimeout(200*time.Millisecond),
		reversetunnel.DirectSite(s.domainName, s.roleAuth),
	)
	c.Assert(err, IsNil)

	apiPort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]

	apiServer := auth.NewAPIWithRoles(authServer, eventsLog, sessionServer, recorder,
		auth.NewAllowAllPermissions(),
		auth.StandardRoles,
	)
	go apiServer.Serve()

	tunAddr := utils.NetAddr{
		AddrNetwork: "tcp", Addr: fmt.Sprintf("127.0.0.1:%v", apiPort),
	}

	s.tunServer, err = auth.NewTunServer(
		tunAddr,
		[]ssh.Signer{s.signer},
		apiServer, authServer, limiter)
	c.Assert(err, IsNil)
	c.Assert(s.tunServer.Start(), IsNil)

	// start handler
	handler, err := NewHandler(Config{
		InsecureHTTPMode: true,
		Proxy:            revTunServer,
		AssetsDir:        "assets/web",
		AuthServers:      tunAddr,
		DomainName:       s.domainName,
	})

	s.webServer = httptest.NewServer(handler)
}

func (s *WebSuite) url() *url.URL {
	u, err := url.Parse("http://" + s.webServer.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	return u
}

func (s *WebSuite) client(opts ...roundtrip.ClientParam) *webClient {
	clt, err := newWebClient(s.url().String(), opts...)
	if err != nil {
		panic(err)
	}
	return clt
}

func (s *WebSuite) TearDownTest(c *C) {
	c.Assert(s.node.Close(), IsNil)
	c.Assert(s.tunServer.Close(), IsNil)
	s.webServer.Close()
}

func (s *WebSuite) TestNewUser(c *C) {
	token, err := s.roleAuth.CreateSignupToken("bob", []string{s.user})
	c.Assert(err, IsNil)

	clt := s.client()
	re, err := clt.Get(clt.Endpoint("webapi", "users", "invites", token), url.Values{})
	c.Assert(err, IsNil)

	var out *renderUserInviteResponse
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)
	c.Assert(out.User, Equals, "bob")
	c.Assert(out.InviteToken, Equals, token)

	_, _, hotpValues, err := s.roleAuth.GetSignupTokenData(token)
	c.Assert(err, IsNil)

	tempPass := "abc123"

	re, err = clt.PostJSON(clt.Endpoint("webapi", "users"), createNewUserReq{
		InviteToken:       token,
		Pass:              tempPass,
		SecondFactorToken: hotpValues[0],
	})
	c.Assert(err, IsNil)

	var sess *createSessionResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sess), IsNil)
	cookies := re.Cookies()
	c.Assert(len(cookies), Equals, 1)

	// now make sure we are logged in by calling authenticated method
	// we need to supply both session cookie and bearer token for
	// request to succeed
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// in absense of session cookie or bearer auth the same request fill fail

	// no session cookie:
	clt = s.client(roundtrip.BearerAuth(sess.Token))
	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(teleport.IsAccessDenied(err), Equals, true)

	// no bearer token:
	clt = s.client(roundtrip.CookieJar(jar))
	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(teleport.IsAccessDenied(err), Equals, true)
}

type authPack struct {
	user    string
	pass    string
	otp     *hotp.HOTP
	session *createSessionResponse
	clt     *webClient
	cookies []*http.Cookie
}

// authPack returns new authenticated package consisting
// of created valid user, hotp token, created web session and
// authenticated client
func (s *WebSuite) authPack(c *C) *authPack {
	user := "bob"
	pass := "abc123"

	hotpURL, _, err := s.roleAuth.UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)
	otp, _, err := hotp.FromURL(hotpURL)
	c.Assert(err, IsNil)
	otp.Increment()

	err = s.roleAuth.UpsertUser(
		services.User{Name: user, AllowedLogins: []string{s.user}})
	c.Assert(err, IsNil)

	clt := s.client()

	re, err := clt.PostJSON(clt.Endpoint("webapi", "sessions"), createSessionReq{
		User:              user,
		Pass:              pass,
		SecondFactorToken: otp.OTP(),
	})
	c.Assert(err, IsNil)

	var sess *createSessionResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sess), IsNil)

	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	return &authPack{
		user:    user,
		pass:    pass,
		session: sess,
		clt:     clt,
		cookies: re.Cookies(),
	}
}

func (s *WebSuite) TestWebSessionsCRUD(c *C) {
	pack := s.authPack(c)

	// make sure we can use client to make authenticated requests
	re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// now delete session
	_, err = pack.clt.Delete(
		pack.clt.Endpoint("webapi", "sessions", pack.session.Token))
	c.Assert(err, IsNil)

	// subsequent requests trying to use this session will fail
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(teleport.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestWebSessionsBadInput(c *C) {
	user := "bob"
	pass := "abc123"

	hotpURL, _, err := s.roleAuth.UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)
	otp, _, err := hotp.FromURL(hotpURL)
	c.Assert(err, IsNil)
	otp.Increment()

	clt := s.client()

	token := otp.OTP()

	reqs := []createSessionReq{
		// emtpy request
		{},
		// missing user
		{
			Pass:              pass,
			SecondFactorToken: token,
		},
		// missing pass
		{
			User:              user,
			SecondFactorToken: token,
		},
		// bad pass
		{
			User:              user,
			Pass:              "bla bla",
			SecondFactorToken: token,
		},
		// bad hotp token
		{
			User:              user,
			Pass:              pass,
			SecondFactorToken: "bad token",
		},
		// missing hotp token
		{
			User: user,
			Pass: pass,
		},
	}
	for i, req := range reqs {
		_, err = clt.PostJSON(clt.Endpoint("webapi", "sessions"), req)
		c.Assert(err, NotNil, Commentf("tc %v", i))
		c.Assert(teleport.IsAccessDenied(err), Equals, true, Commentf("tc %v %T is not access denied", i, err))
	}
}

func (s *WebSuite) TestGetSiteNodes(c *C) {
	pack := s.authPack(c)

	// get site nodes
	re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites", s.domainName, "nodes"), url.Values{})
	c.Assert(err, IsNil)

	var nodes *getSiteNodesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &nodes), IsNil)
	c.Assert(len(nodes.Nodes), Equals, 1)

	// get site nodes using shortcut
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "sites", currentSiteShortcut, "nodes"), url.Values{})
	c.Assert(err, IsNil)

	var nodes2 *getSiteNodesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &nodes2), IsNil)
	c.Assert(len(nodes.Nodes), Equals, 1)

	c.Assert(nodes2, DeepEquals, nodes)
}

func (s *WebSuite) connect(c *C, opts ...string) (*websocket.Conn, *authPack) {
	pack := s.authPack(c)

	var sessionID string
	if len(opts) != 0 {
		sessionID = opts[0]
	}
	u := url.URL{Host: s.url().Host, Scheme: "ws", Path: fmt.Sprintf("/v1/webapi/sites/%v/connect", currentSiteShortcut)}
	data, err := json.Marshal(connectReq{
		Addr:      s.srvAddress,
		Login:     s.user,
		Term:      connectTerm{W: 100, H: 100},
		SessionID: sessionID,
	})
	c.Assert(err, IsNil)

	q := u.Query()
	q.Set("params", string(data))
	q.Set(roundtrip.AccessTokenQueryParam, pack.session.Token)
	u.RawQuery = q.Encode()

	wscfg, err := websocket.NewConfig(u.String(), "http://localhost")
	c.Assert(err, IsNil)
	for _, cookie := range pack.cookies {
		wscfg.Header.Add("Cookie", cookie.String())
	}
	clt, err := websocket.DialConfig(wscfg)
	c.Assert(err, IsNil)

	return clt, pack
}

func (s *WebSuite) TestConnect(c *C) {
	clt, _ := s.connect(c)
	defer clt.Close()

	doneC := make(chan error, 2)
	go func() {
		_, err := io.WriteString(clt, "expr 137 + 39\r\nexit\r\n")
		doneC <- err
	}()

	output := &bytes.Buffer{}
	go func() {
		_, err := io.Copy(output, clt)
		doneC <- err
	}()

	timeoutC := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-doneC:
			break
		case <-timeoutC:
			c.Fatalf("timeout!")
		}
	}

	c.Assert(removeSpace(output.String()), Matches, ".*176.*")
}

func (s *WebSuite) TestNodesWithSessions(c *C) {
	sid := "testsession"
	clt, pack := s.connect(c, sid)
	defer clt.Close()

	// to make sure we have a session
	_, err := io.WriteString(clt, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	// make sure server has replied
	out := make([]byte, 100)
	clt.Read(out)
	fmt.Printf("%v", string(out))

	var nodes *getSiteNodesResponse
	for i := 0; i < 3; i++ {
		// get site nodes and make sure the node has our active party
		re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites", s.domainName, "nodes"), url.Values{})
		c.Assert(err, IsNil)

		c.Assert(json.Unmarshal(re.Bytes(), &nodes), IsNil)
		c.Assert(len(nodes.Nodes), Equals, 1)

		if len(nodes.Nodes[0].Sessions) == 1 {
			break
		}
		// sessions do not appear momentarily as there's async heartbeat
		// procedure
		time.Sleep(20 * time.Millisecond)
	}

	c.Assert(len(nodes.Nodes[0].Sessions), Equals, 1)
	c.Assert(nodes.Nodes[0].Sessions[0].ID, Equals, sid)
}

func removeSpace(in string) string {
	for _, c := range []string{"\n", "\r", "\t"} {
		in = strings.Replace(in, c, " ", -1)
	}
	return strings.TrimSpace(in)
}
