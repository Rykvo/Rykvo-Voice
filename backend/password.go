package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
)

const passwordIterations = 600000

func passwordHash(password string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, 32)
}

func newPassword(password string) (salt, hash []byte, err error) {
	salt = make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return
	}
	hash, err = passwordHash(password, salt, passwordIterations)
	return
}

func verifyPassword(password string, salt, expected []byte, iterations int) bool {
	if len(salt) != 16 || len(expected) != 32 || iterations < passwordIterations || iterations > 2000000 {
		return false
	}
	hash, err := passwordHash(password, salt, iterations)
	return err == nil && subtle.ConstantTimeCompare(hash, expected) == 1
}
