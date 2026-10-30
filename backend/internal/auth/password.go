package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// MaxPasswordBytes bcrypt'in sınırıdır; daha uzun parolanın fazlası sessizce
// yok sayılacağı için reddedilir.
const MaxPasswordBytes = 72

func HashPassword(password string, cost int) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", errors.New("parola 72 bayttan uzun olamaz")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	return string(h), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash kullanıcı bulunamadığında da bcrypt karşılaştırması yapmak içindir;
// aksi halde yanıt süresinden e-postanın kayıtlı olup olmadığı anlaşılır.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("zamanlama-esitleme"), bcrypt.DefaultCost)

func burnPasswordCheck(password string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}
