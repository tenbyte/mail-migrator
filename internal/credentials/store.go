package credentials

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const serviceName = "com.tenbyte.mail-migrator"

type Store struct{}

func (Store) Set(id, password string) error {
	if id == "" || password == "" {
		return errors.New("credential id and password are required")
	}
	return keyring.Set(serviceName, id, password)
}

func (Store) Get(id string) (string, error) {
	if id == "" {
		return "", errors.New("credential id is required")
	}
	password, err := keyring.Get(serviceName, id)
	if err != nil {
		return "", fmt.Errorf("read operating system credential store: %w", err)
	}
	return password, nil
}

func (Store) Delete(id string) error {
	if id == "" {
		return nil
	}
	return keyring.Delete(serviceName, id)
}

func (Store) DeleteAll() error {
	err := keyring.DeleteAll(serviceName)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
