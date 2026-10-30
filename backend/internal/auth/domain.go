// Package auth kimlik doğrulama (parola + JWT access/refresh) ve rol tabanlı
// yetkilendirmeyi (RBAC) sağlar.
//
// Bu dosya saf alan kurallarıdır: veritabanı ve HTTP bilmez.
package auth

import (
	"errors"
	"net/mail"
	"strings"
)

type Role string

const (
	RoleCustomer   Role = "customer"
	RoleSeller     Role = "seller"
	RoleOperations Role = "operations"
	RoleFinance    Role = "finance"
	RoleCarrier    Role = "carrier"
	RoleAdmin      Role = "admin"
)

var allRoles = []Role{RoleCustomer, RoleSeller, RoleOperations, RoleFinance, RoleCarrier, RoleAdmin}

func (r Role) Valid() bool {
	for _, x := range allRoles {
		if r == x {
			return true
		}
	}
	return false
}

// Principal kimliği doğrulanmış isteğin sahibidir; access token'dan çıkarılır.
type Principal struct {
	UserID string
	Role   Role
}

type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	Role     Role   `json:"role"`
}

var (
	// ErrInvalidCredentials bilinmeyen e-posta, yanlış parola ve devre dışı
	// hesap için aynıdır; hangi e-postaların kayıtlı olduğu sızdırılmaz.
	ErrInvalidCredentials = errors.New("e-posta ya da parola hatalı")
	ErrInvalidToken       = errors.New("oturum geçersiz ya da süresi dolmuş")
	ErrTokenReuse         = errors.New("oturum token'ı yeniden kullanıldı; oturum sonlandırıldı")
)

// LoginInput sunucu tarafı doğrulaması yapılmış giriş isteğidir.
type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Normalize e-postayı karşılaştırma için küçük harfe çevirir ve alanları doğrular.
// Dönen map alan adı → hata mesajıdır; boşsa girdi geçerlidir.
func (in *LoginInput) Normalize() map[string]string {
	errs := map[string]string{}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if in.Email == "" {
		errs["email"] = "zorunlu"
	} else if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
		errs["email"] = "geçerli bir e-posta değil"
	}
	if in.Password == "" {
		errs["password"] = "zorunlu"
	} else if len(in.Password) > MaxPasswordBytes {
		errs["password"] = "çok uzun"
	}
	return errs
}
