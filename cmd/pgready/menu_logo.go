package main

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
)

const (
	menuLogoColumns = 5
	menuLogoRows    = 2
	kittyChunkSize  = 4096
)

// The original PostgreSQL Slonik from
// https://www.postgresql.org/media/img/about/press/elephant.png
//
//go:embed assets/postgresql-elephant.png
var postgresqlLogo []byte

// Place the official PNG via the Kitty graphics protocol, preserving the cursor.
func (a *app) drawMenuLogo() {
	if a.logoID != 0 {
		fmt.Fprintf(a.out, "\x1b_Ga=p,i=%d,c=%d,r=%d,C=1,q=2;\x1b\\", a.logoID, menuLogoColumns, menuLogoRows)
		return
	}
	a.logoID = rand.Uint32() | 1
	payload := base64.StdEncoding.EncodeToString(postgresqlLogo)
	metadata := fmt.Sprintf("a=T,f=100,i=%d,c=%d,r=%d,C=1,q=2,", a.logoID, menuLogoColumns, menuLogoRows)
	for len(payload) > kittyChunkSize {
		fmt.Fprintf(a.out, "\x1b_G%sm=1;%s\x1b\\", metadata, payload[:kittyChunkSize])
		payload = payload[kittyChunkSize:]
		metadata = ""
	}
	fmt.Fprintf(a.out, "\x1b_G%sm=0;%s\x1b\\", metadata, payload)
}
