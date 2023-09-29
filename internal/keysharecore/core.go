package keysharecore

import (
	"crypto/rand"
	"crypto/rsa"

	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/gabi/gabikeys"
	irma "github.com/privacybydesign/irmago"
)

const (
	JWTIssuerDefault    = "keyshare_server"
	JWTPinExpiryDefault = 5 * 60 // seconds
)

type (
	AESKey [32]byte

	Core struct {
		// Keys used for storage encryption/decryption
		decryptionKeys  map[uint32]AESKey
		decryptionKey   AESKey
		decryptionKeyID uint32

		// Key used to sign keyshare protocol messages
		jwtPrivateKey   *rsa.PrivateKey
		jwtPrivateKeyID uint32

		jwtIssuer    string
		jwtPinExpiry int

		storage ConsistentStorage

		// IRMA issuer keys that are allowed to be used in keyshare
		//  sessions
		trustedKeys map[irma.PublicKeyIdentifier]*gabikeys.PublicKey
	}

	Configuration struct {
		// Keys used for storage encryption/decryption
		DecryptionKey   AESKey
		DecryptionKeyID uint32

		// Key used to sign keyshare protocol messages
		JWTPrivateKey   *rsa.PrivateKey
		JWTPrivateKeyID uint32

		JWTIssuer    string
		JWTPinExpiry int // in seconds

		Storage ConsistentStorage
	}

	ConsistentStorage interface {
		StoreCommitment(id uint64, commitment *big.Int) error
		ConsumeCommitment(id uint64) (*big.Int, error)
		StoreAuthChallenge(id []byte, challenge []byte) error
		ConsumeAuthChallenge(id []byte) ([]byte, error)
	}
)

func NewKeyshareCore(conf *Configuration) *Core {
	c := &Core{
		decryptionKeys: map[uint32]AESKey{},
		trustedKeys:    map[irma.PublicKeyIdentifier]*gabikeys.PublicKey{},
		storage:        conf.Storage,
	}

	c.setDecryptionKey(conf.DecryptionKeyID, conf.DecryptionKey)
	c.setJWTPrivateKey(conf.JWTPrivateKeyID, conf.JWTPrivateKey)

	c.jwtIssuer = conf.JWTIssuer
	if c.jwtIssuer == "" {
		c.jwtIssuer = JWTIssuerDefault
	}
	c.jwtPinExpiry = conf.JWTPinExpiry
	if c.jwtPinExpiry == 0 {
		c.jwtPinExpiry = JWTPinExpiryDefault
	}

	return c
}

func GenerateDecryptionKey() (AESKey, error) {
	var res AESKey
	_, err := rand.Read(res[:])
	return res, err
}

// DangerousAddDecryptionKey adds an AES key for decryption, with identifier keyID.
// Calling this will cause all keyshare secrets generated with the key to be trusted.
func (c *Core) DangerousAddDecryptionKey(keyID uint32, key AESKey) {
	c.decryptionKeys[keyID] = key
}

// Set the aes key for encrypting new/changed keyshare data
// with identifier keyid
// Calling this will also cause all keyshare user secrets generated with the key to be trusted
func (c *Core) setDecryptionKey(keyID uint32, key AESKey) {
	c.decryptionKeys[keyID] = key
	c.decryptionKey = key
	c.decryptionKeyID = keyID
}

// Set key used to sign keyshare protocol messages
func (c *Core) setJWTPrivateKey(id uint32, key *rsa.PrivateKey) {
	c.jwtPrivateKey = key
	c.jwtPrivateKeyID = id
}

// DangerousAddTrustedPublicKey adds a public key as trusted by keysharecore.
// Calling this on incorrectly generated key material WILL compromise keyshare secrets!
func (c *Core) DangerousAddTrustedPublicKey(keyID irma.PublicKeyIdentifier, key *gabikeys.PublicKey) {
	c.trustedKeys[keyID] = key
}
