//go:build darwin && cgo

package notify

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>
static SecKeychainRef testKeychain = NULL;
static CFMutableDictionaryRef keyQuery(void) {
    CFMutableDictionaryRef q = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFDictionarySetValue(q,kSecClass,kSecClassGenericPassword);
    CFDictionarySetValue(q,kSecAttrService,CFSTR("com.vohive.ec20.mac.mail-encryption"));
    CFDictionarySetValue(q,kSecAttrAccount,CFSTR("local-mail-key-v1"));
    if(testKeychain) CFDictionarySetValue(q,kSecUseKeychain,testKeychain);
    return q;
}
static int loadMailKey(unsigned char *out, bool interactive) {
    CFMutableDictionaryRef q=keyQuery();
    if(testKeychain) {
        CFDictionaryRemoveValue(q,kSecUseKeychain);
        const void *items[]={testKeychain};
        CFArrayRef list=CFArrayCreate(NULL,items,1,&kCFTypeArrayCallBacks);
        CFDictionarySetValue(q,kSecMatchSearchList,list);CFRelease(list);
    }
    CFDictionarySetValue(q,kSecReturnData,kCFBooleanTrue);
    CFDictionarySetValue(q,kSecMatchLimit,kSecMatchLimitOne);
    // Preserve the existing file-based item and its ACL. Startup may not prompt.
    Boolean previous=true;
    if(!interactive) {
        CFDictionarySetValue(q,kSecUseAuthenticationUI,kSecUseAuthenticationUIFail);
        OSStatus status=SecKeychainGetUserInteractionAllowed(&previous);
        if(status!=errSecSuccess) { CFRelease(q); return status; }
        status=SecKeychainSetUserInteractionAllowed(false);
        if(status!=errSecSuccess) { CFRelease(q); return status; }
    }
    CFTypeRef result=NULL;
    OSStatus status=SecItemCopyMatching(q,&result); CFRelease(q);
    if(!interactive) SecKeychainSetUserInteractionAllowed(previous);
    if(status!=errSecSuccess) return status;
    if(CFGetTypeID(result)!=CFDataGetTypeID() || CFDataGetLength((CFDataRef)result)!=32) { CFRelease(result); return -50; }
    memcpy(out,CFDataGetBytePtr((CFDataRef)result),32); CFRelease(result); return 0;
}
static int addMailKey(unsigned char *key) {
    CFMutableDictionaryRef q=keyQuery();
    CFDataRef data=CFDataCreate(NULL,key,32);
    CFDictionarySetValue(q,kSecValueData,data);
    CFDictionarySetValue(q,kSecAttrLabel,CFSTR("蜂信 短信转发邮箱加密密钥"));
    OSStatus status=SecItemAdd(q,NULL); CFRelease(data); CFRelease(q); return status;
}
static int openTestKeychain(const char *path) {
    const char *password="isolated-test-keychain";
    return SecKeychainCreate(path,strlen(password),password,false,NULL,&testKeychain);
}
static void closeTestKeychain(void) { if(testKeychain) { SecKeychainDelete(testKeychain); CFRelease(testKeychain); testKeychain=NULL; } }
*/
import "C"

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

var mailKeyMu sync.Mutex

func mailCipher(create, interactive bool) (cipher.AEAD, error) {
	mailKeyMu.Lock()
	defer mailKeyMu.Unlock()
	key := make([]byte, 32)
	status := C.loadMailKey((*C.uchar)(unsafe.Pointer(&key[0])), C.bool(interactive))
	if status == -25300 && create {
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		status = C.addMailKey((*C.uchar)(unsafe.Pointer(&key[0])))
		if status == -25299 {
			status = C.loadMailKey((*C.uchar)(unsafe.Pointer(&key[0])), C.bool(interactive))
		}
	}
	if status != 0 {
		return nil, fmt.Errorf("macOS Keychain error %d", int(status))
	}
	block, err := aes.NewCipher(key)
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func protect(secret []byte) ([]byte, error) {
	aead, err := mailCipher(true, true)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	header := []byte("VHMAC1")
	return append(append(header, nonce...), aead.Seal(nil, nonce, secret, header)...), nil
}
func unprotect(data []byte) ([]byte, error) {
	return unseal(data, false)
}
func unprotectInteractive(data []byte) ([]byte, error) {
	return unseal(data, true)
}
func unseal(data []byte, interactive bool) ([]byte, error) {
	if len(data) < 6 || string(data[:6]) != "VHMAC1" {
		return nil, errors.New("unsupported mailbox secret format")
	}
	aead, err := mailCipher(false, interactive)
	if err != nil {
		return nil, err
	}
	if len(data) < 6+aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("truncated mailbox secret")
	}
	return aead.Open(nil, data[6:6+aead.NonceSize()], data[6+aead.NonceSize():], data[:6])
}
func startIsolatedKeychain(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if s := C.openTestKeychain(p); s != 0 {
		return fmt.Errorf("test keychain: %d", int(s))
	}
	return nil
}
func stopIsolatedKeychain() { C.closeTestKeychain() }
