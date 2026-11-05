package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func certificate(host string, now time.Time) ([]byte, []byte, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, nil, fmt.Errorf("host must be an IP address")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "EC20 Mobile Gateway"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{ip, net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), nil
}

func reusable(certPath, keyPath, host string, now time.Time) bool {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	return err == nil && cert.VerifyHostname(host) == nil && !now.Before(cert.NotBefore) && cert.NotAfter.After(now.AddDate(0, 1, 0))
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mobile-setup-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func protectDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(dir, 0700)
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	cmd := exec.Command("icacls.exe", dir, "/inheritance:r", "/grant:r", "*"+u.Uid+":(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F")
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("protect mobile credentials: %w", err)
	}
	return nil
}

func configure(configPath, host string) error {
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() || ip.IsLoopback() {
		return fmt.Errorf("choose this computer's private LAN IPv4 address")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	local := false
	for _, address := range addresses {
		if n, ok := address.(*net.IPNet); ok && n.IP.Equal(ip) {
			local = true
		}
	}
	if !local {
		return fmt.Errorf("the chosen address is not assigned to this computer")
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err = json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &cfg); err != nil {
		return err
	}
	web, _ := cfg["web_listen"].(string)
	webHost, _, err := net.SplitHostPort(web)
	if err != nil || net.ParseIP(webHost) == nil || !net.ParseIP(webHost).IsLoopback() {
		return fmt.Errorf("the administration web interface must remain loopback-only")
	}
	privateDir := filepath.Join(filepath.Dir(configPath), "data", "mobile")
	if err = protectDirectory(privateDir); err != nil {
		return err
	}
	backup := filepath.Join(privateDir, "config-before-"+time.Now().Format("20060102-150405.000000000")+".json")
	if err = writePrivate(backup, raw); err != nil {
		return err
	}
	certPath, keyPath := filepath.Join(privateDir, "server.pem"), filepath.Join(privateDir, "server-key.pem")
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(filepath.Dir(configPath), value)
	}
	if configured, ok := cfg["mobile_cert_file"].(string); ok && configured != "" {
		certPath = resolve(configured)
	}
	if configured, ok := cfg["mobile_key_file"].(string); ok && configured != "" {
		keyPath = resolve(configured)
	}
	if !reusable(certPath, keyPath, host, time.Now()) {
		cert, key, certErr := certificate(host, time.Now())
		if certErr != nil {
			return certErr
		}
		// Never replace the certificate referenced by the previous config.
		// Restoring that config must also restore its original Android pin.
		var suffix [9]byte
		if _, err = rand.Read(suffix[:]); err != nil {
			return err
		}
		name := "server-" + time.Now().Format("20060102-150405") + "-" + base64.RawURLEncoding.EncodeToString(suffix[:])
		certPath = filepath.Join(privateDir, name+".pem")
		keyPath = filepath.Join(privateDir, name+"-key.pem")
		if err = writePrivate(keyPath, key); err != nil {
			return err
		}
		if err = writePrivate(certPath, cert); err != nil {
			return err
		}
	}
	token, _ := cfg["mobile_token"].(string)
	if len(token) < 32 {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			return err
		}
		token = base64.RawURLEncoding.EncodeToString(secret[:])
	}
	cfg["mobile_listen"] = net.JoinHostPort(host, "7577")
	cfg["mobile_advertise_host"] = host
	cfg["mobile_token"] = token
	cfg["mobile_cert_file"] = certPath
	cfg["mobile_key_file"] = keyPath
	cfg["sip_listen"] = "0.0.0.0:5068"
	cfg["sip_advertise_ip"] = host
	updated, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err = writePrivate(configPath, append(updated, '\n')); err != nil {
		return err
	}
	fmt.Println("Android API configured at https://" + net.JoinHostPort(host, "7577"))
	fmt.Println("Pair from the authenticated local web page. No credentials were printed.")
	return nil
}

func main() {
	config := flag.String("config", "config.json", "Existing gateway configuration")
	host := flag.String("host", "", "This computer's private LAN IPv4 address")
	flag.Parse()
	if err := configure(*config, *host); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
