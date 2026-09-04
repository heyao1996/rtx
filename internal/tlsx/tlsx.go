// Package tlsx — rtx TLS 信道支持：自签证书生成 + 证书指纹 pin 校验。
// server 用自签证书（首次运行自动生成落盘），agent 用 sha256 指纹 pin
// 校验服务端（防 MITM，无需 CA 体系）。全标准库。
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// ServerConfig 返回 server 侧 tls.Config。certFile/keyFile 不存在时自动生成自签。
// 返回 (config, 证书指纹hex, error)
func ServerConfig(certFile, keyFile string) (*tls.Config, string, error) {
	cert, err := loadOrGen(certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return nil, "", err
	}
	// 指纹 = 证书 DER sha256（从 PEM 解出 DER）
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, "", fmt.Errorf("bad cert pem")
	}
	sum := sha256.Sum256(blk.Bytes)
	fp := hex.EncodeToString(sum[:])

	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	return cfg, fp, nil
}

// ClientConfig 返回 agent 侧 tls.Config：pin 校验服务端证书指纹。
func ClientConfig(pinHex string) (*tls.Config, error) {
	pin, err := hex.DecodeString(pinHex)
	if err != nil || len(pin) != sha256.Size {
		return nil, fmt.Errorf("bad pin (want 64 hex chars)")
	}
	return &tls.Config{
		InsecureSkipVerify: true, // 自签，跳过 CA 链；用 pin 指纹兜底
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no peer cert")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != hex.EncodeToString(pin) {
				return fmt.Errorf("cert pin mismatch")
			}
			return nil
		},
	}, nil
}

// WrapServer 把明文监听包装成 TLS 监听（若启用）
func WrapServer(ln net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(ln, cfg)
}

func loadOrGen(certFile, keyFile string) (tls.Certificate, error) {
	if _, err := os.Stat(certFile); err == nil {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	// 生成自签
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "svc"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}
