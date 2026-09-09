package main

import (
	"math/rand"
	"time"

	// Embed the IANA time zone database.
	//
	// Windows ships no zoneinfo, so without this time.LoadLocation only accepts
	// "UTC" and "Local" and every real zone name is rejected — which would make
	// system.timezone unusable, since the value handed to servers as TZ has to be
	// an IANA name for the JVM and Node to understand it.
	//
	// Costs roughly 450KB in the binary.
	_ "time/tzdata"

	"github.com/pterodactyl/wings/cmd"
)

func main() {
	// Since we make use of the math/rand package in the code, especially for generating
	// non-cryptographically secure random strings we need to seed the RNG. Just make use
	// of the current time for this.
	rand.Seed(time.Now().UnixNano())

	// Execute the main binary code.
	cmd.Execute()
}
