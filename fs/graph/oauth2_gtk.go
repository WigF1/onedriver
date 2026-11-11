//go:build linux && cgo
// +build linux,cgo

package graph

/*
#cgo linux pkg-config: webkit2gtk-4.1
#include "stdlib.h"
#include "oauth2_gtk.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"github.com/rs/zerolog/log"
)

// Fetch the auth code required as the first part of oauth2 authentication. Uses
// webkit2gtk to create a popup browser.
func getAuthCode(a AuthConfig, accountName string) (string, error) {
	runtime.LockOSThread() // GTK must be driven from one thread
	defer runtime.UnlockOSThread()
	warnMissingGstreamerPlugins()
	cAuthURL := C.CString(getAuthURL(a))
	cAccountName := C.CString(accountName)
	cResponse := C.webkit_auth_window(cAuthURL, cAccountName)
	response := C.GoString(cResponse)
	C.free(unsafe.Pointer(cAuthURL))
	C.free(unsafe.Pointer(cAccountName))
	C.free(unsafe.Pointer(cResponse))

	code, err := parseAuthCode(response)
	if err != nil || code == "" {
		fmt.Fprint(os.Stderr, "\nLogin did not complete (the window was closed, or the "+
			"login page did not work correctly, e.g. the Authenticator number was not "+
			"shown).\nTry again with:  onedriver -n\n"+
			"which lets you log in using your normal web browser. To debug the "+
			"embedded login window, run with ONEDRIVER_DEBUG_AUTH=1.\n")
		return "", errors.New("login did not complete: no validation code returned")
	}
	return code, nil
}

// uriGetHost is exclusively here for testing because we cannot use CGo in tests,
// but can use functions that invoke CGo in tests.
func uriGetHost(uri string) string {
	input := C.CString(uri)
	defer C.free(unsafe.Pointer(input))

	host := C.uri_get_host(input)
	defer C.free(unsafe.Pointer(host))
	if host == nil {
		return ""
	}
	return C.GoString(host)
}

// warnMissingGstreamerPlugins warns if GStreamer's autoaudiosink element is
// missing (gst-plugins-good). Without it, WebKitGTK's web process hits a NULL
// pointer while setting up the Microsoft login page and the Authenticator
// number matching code is never shown.
func warnMissingGstreamerPlugins() {
	inspect, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		return // can't tell, don't guess
	}
	if exec.Command(inspect, "--exists", "autoaudiosink").Run() != nil {
		log.Warn().Msg("GStreamer element autoaudiosink not found. The login window " +
			"may not show the Microsoft Authenticator number. Install gst-plugins-good " +
			"(gstreamer1.0-plugins-good on Debian/Ubuntu) or use onedriver -n.")
	}
}
