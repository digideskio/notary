package cryptoservice

import (
	"archive/zip"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/notary"
	"github.com/docker/notary/trustmanager"
)

const zipMadeByUNIX = 3 << 8

var (
	// ErrNoValidPrivateKey is returned if a key being imported doesn't
	// look like a private key
	ErrNoValidPrivateKey = errors.New("no valid private key found")

	// ErrRootKeyNotEncrypted is returned if a root key being imported is
	// unencrypted
	ErrRootKeyNotEncrypted = errors.New("only encrypted root keys may be imported")

	// ErrNoKeysFoundForGUN is returned if no keys are found for the
	// specified GUN during export
	ErrNoKeysFoundForGUN = errors.New("no keys found for specified GUN")
)

// ExportKey exports the specified private key to an io.Writer in PEM format.
// The key's existing encryption is preserved.
func (cs *CryptoService) ExportKey(dest io.Writer, keyID, role string) error {
	var (
		pemBytes []byte
		err      error
	)

	for _, ks := range cs.keyStores {
		pemBytes, err = ks.ExportKey(keyID)
		if err != nil {
			continue
		}
	}
	if err != nil {
		return err
	}

	nBytes, err := dest.Write(pemBytes)
	if err != nil {
		return err
	}
	if nBytes != len(pemBytes) {
		return errors.New("Unable to finish writing exported key.")
	}
	return nil
}

// ExportKeyReencrypt exports the specified private key to an io.Writer in
// PEM format. The key is reencrypted with a new passphrase.
func (cs *CryptoService) ExportKeyReencrypt(dest io.Writer, keyID string, newPassphraseRetriever notary.PassRetriever) error {
	privateKey, _, err := cs.GetPrivateKey(keyID)
	if err != nil {
		return err
	}

	keyInfo, err := cs.GetKeyInfo(keyID)
	if err != nil {
		return err
	}

	// Create temporary keystore to use as a staging area
	tempBaseDir, err := ioutil.TempDir("", "notary-key-export-")
	defer os.RemoveAll(tempBaseDir)

	tempKeyStore, err := trustmanager.NewKeyFileStore(tempBaseDir, newPassphraseRetriever)
	if err != nil {
		return err
	}

	err = tempKeyStore.AddKey(keyInfo, privateKey)
	if err != nil {
		return err
	}

	pemBytes, err := tempKeyStore.ExportKey(keyID)
	if err != nil {
		return err
	}
	nBytes, err := dest.Write(pemBytes)
	if err != nil {
		return err
	}
	if nBytes != len(pemBytes) {
		return errors.New("Unable to finish writing exported key.")
	}
	return nil
}

// ExportAllKeys exports all keys to an io.Writer in zip format.
// newPassphraseRetriever will be used to obtain passphrases to use to encrypt the existing keys.
func (cs *CryptoService) ExportAllKeys(dest io.Writer, newPassphraseRetriever notary.PassRetriever) error {
	tempBaseDir, err := ioutil.TempDir("", "notary-key-export-")
	defer os.RemoveAll(tempBaseDir)

	// Create temporary keystore to use as a staging area
	tempKeyStore, err := trustmanager.NewKeyFileStore(tempBaseDir, newPassphraseRetriever)
	if err != nil {
		return err
	}

	for _, ks := range cs.keyStores {
		if err := moveKeys(ks, tempKeyStore); err != nil {
			return err
		}
	}

	zipWriter := zip.NewWriter(dest)

	if err := addKeysToArchive(zipWriter, tempKeyStore); err != nil {
		return err
	}

	zipWriter.Close()

	return nil
}

// ImportKeysZip imports keys from a zip file provided as an zip.Reader. The
// keys in the root_keys directory are left encrypted, but the other keys are
// decrypted with the specified passphrase.
func (cs *CryptoService) ImportKeysZip(zipReader zip.Reader, retriever notary.PassRetriever) error {
	// Temporarily store the keys in maps, so we can bail early if there's
	// an error (for example, wrong passphrase), without leaving the key
	// store in an inconsistent state
	newKeys := make(map[string][]byte)

	// Iterate through the files in the archive. Don't add the keys
	for _, f := range zipReader.File {
		fNameTrimmed := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		fileBytes, err := ioutil.ReadAll(rc)
		if err != nil {
			return nil
		}

		// Note that using / as a separator is okay here - the zip
		// package guarantees that the separator will be /
		if fNameTrimmed[len(fNameTrimmed)-5:] == "_root" {
			if err = CheckRootKeyIsEncrypted(fileBytes); err != nil {
				return err
			}
		}
		newKeys[fNameTrimmed] = fileBytes
	}

	for keyName, pemBytes := range newKeys {
		// Get the key role information as well as its data.PrivateKey representation
		_, keyInfo, err := trustmanager.KeyInfoFromPEM(pemBytes, keyName)
		if err != nil {
			return err
		}
		privKey, err := trustmanager.ParsePEMPrivateKey(pemBytes, "")
		if err != nil {
			privKey, _, err = trustmanager.GetPasswdDecryptBytes(retriever, pemBytes, "", "imported "+keyInfo.Role)
			if err != nil {
				return err
			}
		}
		// Add the key to our cryptoservice, will add to the first successful keystore
		if err = cs.AddKey(keyInfo.Role, keyInfo.Gun, privKey); err != nil {
			return err
		}
	}

	return nil
}

// ExportKeysByGUN exports all keys associated with a specified GUN to an
// io.Writer in zip format. passphraseRetriever is used to select new passphrases to use to
// encrypt the keys.
func (cs *CryptoService) ExportKeysByGUN(dest io.Writer, gun string, passphraseRetriever notary.PassRetriever) error {
	tempBaseDir, err := ioutil.TempDir("", "notary-key-export-")
	defer os.RemoveAll(tempBaseDir)

	// Create temporary keystore to use as a staging area
	tempKeyStore, err := trustmanager.NewKeyFileStore(tempBaseDir, passphraseRetriever)
	if err != nil {
		return err
	}

	for _, ks := range cs.keyStores {
		if err := moveKeysByGUN(ks, tempKeyStore, gun); err != nil {
			return err
		}
	}

	zipWriter := zip.NewWriter(dest)

	if len(tempKeyStore.ListKeys()) == 0 {
		return ErrNoKeysFoundForGUN
	}

	if err := addKeysToArchive(zipWriter, tempKeyStore); err != nil {
		return err
	}

	zipWriter.Close()

	return nil
}

func moveKeysByGUN(oldKeyStore, newKeyStore trustmanager.KeyStore, gun string) error {
	for keyID, keyInfo := range oldKeyStore.ListKeys() {
		// Skip keys that aren't associated with this GUN
		if keyInfo.Gun != gun {
			continue
		}

		privKey, _, err := oldKeyStore.GetKey(keyID)
		if err != nil {
			return err
		}

		err = newKeyStore.AddKey(keyInfo, privKey)
		if err != nil {
			return err
		}
	}

	return nil
}

func moveKeys(oldKeyStore, newKeyStore trustmanager.KeyStore) error {
	for keyID, keyInfo := range oldKeyStore.ListKeys() {
		privateKey, _, err := oldKeyStore.GetKey(keyID)
		if err != nil {
			return err
		}

		err = newKeyStore.AddKey(keyInfo, privateKey)

		if err != nil {
			return err
		}
	}

	return nil
}

func addKeysToArchive(zipWriter *zip.Writer, newKeyStore *trustmanager.KeyFileStore) error {
	for _, relKeyPath := range newKeyStore.ListFiles() {
		fullKeyPath, err := newKeyStore.GetPath(relKeyPath)
		if err != nil {
			return err
		}

		fi, err := os.Lstat(fullKeyPath)
		if err != nil {
			return err
		}

		infoHeader, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(newKeyStore.BaseDir(), fullKeyPath)
		if err != nil {
			return err
		}
		infoHeader.Name = relPath

		zipFileEntryWriter, err := zipWriter.CreateHeader(infoHeader)
		if err != nil {
			return err
		}

		fileContents, err := ioutil.ReadFile(fullKeyPath)
		if err != nil {
			return err
		}

		if _, err = zipFileEntryWriter.Write(fileContents); err != nil {
			return err
		}
	}

	return nil
}

// CheckRootKeyIsEncrypted makes sure the root key is encrypted. We have
// internal assumptions that depend on this.
func CheckRootKeyIsEncrypted(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ErrNoValidPrivateKey
	}

	if !x509.IsEncryptedPEMBlock(block) {
		return ErrRootKeyNotEncrypted
	}

	return nil
}
