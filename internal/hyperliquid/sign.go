package hyperliquid

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// Hyperliquid signs two ways. Exchange actions (orders, cancels, leverage)
// are hashed as MessagePack plus the nonce, and the hash is signed as an
// EIP-712 "Agent" struct; an agent wallet may sign these for the account
// that approved it. Account actions (approving an agent or a builder fee,
// transfers) are EIP-712 structs the account's own wallet signs.

// Signer is a wallet: the agent key the engine holds for a user.
type Signer struct {
	key     *secp256k1.PrivateKey
	address string
}

// NewSigner takes a 32-byte private key in hex.
func NewSigner(hexKey string) (*Signer, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hexKey), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("a wallet key is 32 bytes in hex")
	}
	key := secp256k1.PrivKeyFromBytes(raw)
	if key.Key.IsZero() {
		return nil, errors.New("invalid wallet key")
	}
	return &Signer{key: key, address: addressOf(key)}, nil
}

// GenerateSigner makes a fresh wallet and returns it with its key in hex,
// which is the secret to seal in the vault.
func GenerateSigner() (*Signer, string, error) {
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, "", err
	}
	return &Signer{key: key, address: addressOf(key)}, "0x" + hex.EncodeToString(key.Serialize()), nil
}

func addressOf(key *secp256k1.PrivateKey) string {
	pub := key.PubKey().SerializeUncompressed()
	return "0x" + hex.EncodeToString(keccak(pub[1:])[12:])
}

// Address is the wallet's address, lowercase.
func (s *Signer) Address() string { return s.address }

// Signature is r, s and v as the exchange expects them.
type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

func (s *Signer) sign(digest []byte) Signature {
	sig := ecdsa.SignCompact(s.key, digest, false)
	return Signature{R: "0x" + new(big.Int).SetBytes(sig[1:33]).Text(16), S: "0x" + new(big.Int).SetBytes(sig[33:65]).Text(16), V: int(sig[0])}
}

func keccak(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// actionHash binds an action to its nonce, the vault it acts for and its
// expiry, exactly as the exchange recomputes it.
func actionHash(action any, vault string, nonce uint64, expiresAfter *uint64) ([]byte, error) {
	data, err := pack(action)
	if err != nil {
		return nil, err
	}
	data = binary.BigEndian.AppendUint64(data, nonce)
	if vault == "" {
		data = append(data, 0)
	} else {
		addr, err := hex.DecodeString(strings.TrimPrefix(vault, "0x"))
		if err != nil || len(addr) != 20 {
			return nil, errors.New("invalid vault address")
		}
		data = append(append(data, 1), addr...)
	}
	if expiresAfter != nil {
		data = binary.BigEndian.AppendUint64(append(data, 0), *expiresAfter)
	}
	return keccak(data), nil
}

// signL1 signs an exchange action. The chain is named by the "source"
// letter, so a testnet signature is void on the live exchange.
func (s *Signer) signL1(action any, vault string, nonce uint64, expiresAfter *uint64, mainnet bool) (Signature, error) {
	h, err := actionHash(action, vault, nonce, expiresAfter)
	if err != nil {
		return Signature{}, err
	}
	source := "b"
	if mainnet {
		source = "a"
	}
	domain := domainSeparator("Exchange", "1", big.NewInt(1337))
	msg := keccak(keccak([]byte("Agent(string source,bytes32 connectionId)")), keccak([]byte(source)), h)
	return s.sign(keccak([]byte{0x19, 0x01}, domain, msg)), nil
}

// field is one member of an EIP-712 struct.
type field struct {
	name, typ string
}

// signatureChainID is the chain the wallet signs account actions on. It
// can be any chain; hyperliquidChain in the action names the environment.
const signatureChainID = "0x66eee"

// typedData is an EIP-712 message as a wallet signs it (eth_signTypedData_v4).
type typedData struct {
	Types       map[string][]map[string]string `json:"types"`
	PrimaryType string                         `json:"primaryType"`
	Domain      map[string]any                 `json:"domain"`
	Message     omap                           `json:"message"`
}

func userTypedData(primary string, fields []field, message omap) typedData {
	types := map[string][]map[string]string{"EIP712Domain": {
		{"name": "name", "type": "string"}, {"name": "version", "type": "string"},
		{"name": "chainId", "type": "uint256"}, {"name": "verifyingContract", "type": "address"}}}
	for _, f := range fields {
		types[primary] = append(types[primary], map[string]string{"name": f.name, "type": f.typ})
	}
	return typedData{Types: types, PrimaryType: primary, Message: message, Domain: map[string]any{
		"name": "HyperliquidSignTransaction", "version": "1", "chainId": chainIDInt(), "verifyingContract": zeroAddress}}
}

const zeroAddress = "0x0000000000000000000000000000000000000000"

func chainIDInt() int64 {
	n, _ := new(big.Int).SetString(strings.TrimPrefix(signatureChainID, "0x"), 16)
	return n.Int64()
}

// userDigest hashes an account action: the struct named primary with the
// given fields, taking values from message by name.
func userDigest(primary string, fields []field, message omap) ([]byte, error) {
	var sig strings.Builder
	sig.WriteString(primary)
	sig.WriteString("(")
	for i, f := range fields {
		if i > 0 {
			sig.WriteString(",")
		}
		sig.WriteString(f.typ)
		sig.WriteString(" ")
		sig.WriteString(f.name)
	}
	sig.WriteString(")")
	enc := keccak([]byte(sig.String()))
	for _, f := range fields {
		var v any
		for _, e := range message {
			if e.k == f.name {
				v = e.v
			}
		}
		word, err := encodeWord(f.typ, v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", primary, f.name, err)
		}
		enc = append(enc, word...)
	}
	domain := domainSeparator("HyperliquidSignTransaction", "1", big.NewInt(chainIDInt()))
	return keccak([]byte{0x19, 0x01}, domain, keccak(enc)), nil
}

func domainSeparator(name, version string, chainID *big.Int) []byte {
	return keccak(keccak([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")),
		keccak([]byte(name)), keccak([]byte(version)), pad32(chainID.Bytes()), make([]byte, 32))
}

// encodeWord gives the 32-byte EIP-712 encoding of one value.
func encodeWord(typ string, v any) ([]byte, error) {
	switch typ {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("not a string")
		}
		return keccak([]byte(s)), nil
	case "uint64":
		switch n := v.(type) {
		case uint64:
			return pad32(new(big.Int).SetUint64(n).Bytes()), nil
		case int64:
			return pad32(big.NewInt(n).Bytes()), nil
		case int:
			return pad32(big.NewInt(int64(n)).Bytes()), nil
		}
		return nil, errors.New("not an integer")
	case "address":
		s, ok := v.(string)
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(s), "0x"))
		if !ok || err != nil || len(raw) != 20 {
			return nil, errors.New("not an address")
		}
		return pad32(raw), nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("not a bool")
		}
		if b {
			return pad32([]byte{1}), nil
		}
		return make([]byte, 32), nil
	case "bytes32":
		raw, ok := v.([]byte)
		if !ok || len(raw) != 32 {
			return nil, errors.New("not 32 bytes")
		}
		return raw, nil
	}
	return nil, fmt.Errorf("unsupported type %s", typ)
}

func pad32(b []byte) []byte {
	if len(b) > 32 {
		b = b[len(b)-32:]
	}
	return append(make([]byte, 32-len(b)), b...)
}

// signUser signs an account action with this wallet. The engine never
// holds users' account wallets; this serves tests and an operator's own
// scripts. Users sign through their wallet from the typed data instead.
func (s *Signer) signUser(primary string, fields []field, message omap) (Signature, error) {
	d, err := userDigest(primary, fields, message)
	if err != nil {
		return Signature{}, err
	}
	return s.sign(d), nil
}
