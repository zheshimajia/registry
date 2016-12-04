package httpd

import (
	"net/http"
	"strings"

	"github.com/lodastack/registry/common"

	"github.com/julienschmidt/httprouter"
)

// UserToken struct
type UserToken struct {
	User  string `json:"user"`
	Token string `json:"token"`
}

// SigninHandler handler signin request
func (s *Service) HandlerSignin(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var err error
	user := strings.ToLower(r.FormValue("username"))
	pass := r.FormValue("password")
	if err = LDAPAuth(user, pass); err != nil {
		ReturnServerError(w, err)
		return
	}
	key := common.GenUUID()
	s.session.Set(key, user)
	ReturnJson(w, 200, UserToken{User: user, Token: key})
}

//SignoutHandler handler signout request
func (s *Service) HandlerSignout(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var user string
	key := r.Header.Get("AuthToken")
	v := s.session.Get(key)
	if v == nil {
		ReturnJson(w, 200, UserToken{Token: key})
		return
	}
	user = v.(string)
	s.session.Delete(key)
	ReturnJson(w, 200, UserToken{User: user, Token: key})
	return
}