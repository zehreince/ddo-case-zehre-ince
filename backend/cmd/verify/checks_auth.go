package main

import (
	"fmt"
	"net/http"

	"carsihub/internal/auth"
	"carsihub/internal/seed"
)

type session struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	User         auth.User `json:"user"`
}

func (c *client) login(email, password string) (session, response, error) {
	r, err := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": email, "password": password})
	if err != nil {
		return session{}, r, err
	}
	var s session
	if r.status == http.StatusOK {
		err = r.decode(&s)
	}
	return s, r, err
}

func (c *client) loginAs(role auth.Role) (session, error) {
	for _, u := range seed.DemoUsers {
		if u.Role == role {
			s, r, err := c.login(u.Email, seed.DemoPassword)
			if err == nil && r.status != http.StatusOK {
				err = fmt.Errorf("%s girişi: %d %s", u.Email, r.status, truncate(r.body))
			}
			return s, err
		}
	}
	return session{}, fmt.Errorf("%s rolünde demo kullanıcı yok", role)
}

var authChecks = []check{
	{"AUTH-1", "her rolün demo hesabıyla giriş yapılır", func(c *client) error {
		for _, u := range seed.DemoUsers {
			s, err := c.loginAs(u.Role)
			if err != nil {
				return err
			}
			if s.User.Role != u.Role || s.AccessToken == "" || s.RefreshToken == "" {
				return fmt.Errorf("%s: oturum eksik", u.Email)
			}
		}
		return nil
	}},
	{"AUTH-2", "yanlış parola 401 invalid_credentials", func(c *client) error {
		_, r, err := c.login(seed.DemoUsers[0].Email, "yanlis-parola")
		if err != nil {
			return err
		}
		return r.expect(http.StatusUnauthorized, "invalid_credentials")
	}},
	{"AUTH-3", "geçersiz girdi 422, alan bazlı hata", func(c *client) error {
		_, r, err := c.login("bozuk-eposta", "")
		if err != nil {
			return err
		}
		var e struct {
			Error struct {
				Fields map[string]string `json:"fields"`
			} `json:"error"`
		}
		if err := r.expect(http.StatusUnprocessableEntity, "validation_failed"); err != nil {
			return err
		}
		if err := r.decode(&e); err != nil || e.Error.Fields["email"] == "" || e.Error.Fields["password"] == "" {
			return fmt.Errorf("alan hataları eksik: %s", truncate(r.body))
		}
		return nil
	}},
	{"AUTH-4", "token'sız ve sahte token'lı istek 401", func(c *client) error {
		for _, tok := range []string{"", "sahte.token.degeri"} {
			r, err := c.get("/api/v1/auth/me", tok)
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusUnauthorized, "unauthorized"); err != nil {
				return err
			}
		}
		return nil
	}},
	{"RBAC-1", "yönetici dışı her rol /admin/users'a 403 alır", func(c *client) error {
		for _, u := range seed.DemoUsers {
			if u.Role == auth.RoleAdmin {
				continue
			}
			s, err := c.loginAs(u.Role)
			if err != nil {
				return err
			}
			r, err := c.get("/api/v1/admin/users", s.AccessToken)
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusForbidden, "forbidden"); err != nil {
				return fmt.Errorf("%s: %w", u.Role, err)
			}
		}
		return nil
	}},
	{"RBAC-2", "yönetici kullanıcıları keyset sayfalamayla listeler", func(c *client) error {
		s, err := c.loginAs(auth.RoleAdmin)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		path := "/api/v1/admin/users?limit=2"
		for range 10 {
			r, err := c.get(path, s.AccessToken)
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusOK, ""); err != nil {
				return err
			}
			var page struct {
				Items      []auth.User `json:"items"`
				NextCursor string      `json:"next_cursor"`
			}
			if err := r.decode(&page); err != nil {
				return err
			}
			for _, u := range page.Items {
				if seen[u.ID] {
					return fmt.Errorf("%s iki sayfada döndü", u.Email)
				}
				seen[u.ID] = true
			}
			if page.NextCursor == "" {
				break
			}
			path = "/api/v1/admin/users?limit=2&cursor=" + page.NextCursor
		}
		if len(seen) < len(seed.DemoUsers) {
			return fmt.Errorf("%d kullanıcı listelendi, en az %d bekleniyordu", len(seen), len(seed.DemoUsers))
		}
		return nil
	}},
	{"AUTH-5", "refresh token döner; eskisi tekrar kullanılırsa oturum kapanır", func(c *client) error {
		s, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		refresh := func(tok string) (response, error) {
			return c.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": tok})
		}
		r, err := refresh(s.RefreshToken)
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusOK, ""); err != nil {
			return err
		}
		var next session
		if err := r.decode(&next); err != nil {
			return err
		}
		if r, err = refresh(s.RefreshToken); err != nil {
			return err
		}
		if err := r.expect(http.StatusUnauthorized, "token_reused"); err != nil {
			return fmt.Errorf("eski token: %w", err)
		}
		if r, err = refresh(next.RefreshToken); err != nil {
			return err
		}
		if err := r.expect(http.StatusUnauthorized, ""); err != nil {
			return fmt.Errorf("aile iptal edilmeliydi: %w", err)
		}
		return nil
	}},
	{"AUTH-6", "çıkış oturumu sonlandırır", func(c *client) error {
		s, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		r, err := c.do(http.MethodPost, "/api/v1/auth/logout", "", map[string]string{"refresh_token": s.RefreshToken})
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusNoContent, ""); err != nil {
			return err
		}
		r, err = c.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken})
		if err != nil {
			return err
		}
		return r.expect(http.StatusUnauthorized, "")
	}},
}
