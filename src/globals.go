/* Jacobin VM -- A Java virtual machine
 * (c) Copyright 2021 by Andrew Binstock. All rights reserved
 * Licensed under Mozilla Public License 2.0
 */

package main

import "time"

// Globals contains variables that need to be globally accessible,
// such as VM and program args, pointers to classloaders, etc.
type Globals struct {
	// ---- jacobin version number ----
	// note: all references to version number must come from this literal
	version string

	// ---- logging items ----
	logLevel  int
	startTime time.Time

	// ---- command-line items ----
	jacobinName string
	args        []string
	commandLine string

	// ---- classloading items ----
	/*

	   // ---- command-line items ----
	   var commandLine: String = ""
	   var startingClass = ""
	   var appArgs: [String] = [""]

	   // ---- classloading items ----
	   var bootstrapLoader = Classloader( name: "bootstrap", parent: "" )
	   var systemLoader    = Classloader( name: "system", parent: "bootstrap" )
	   var assertionStatus = true //default assertion status is that assertions are executed. This is only for start-up.
	   var verifyBytecode  = verifyLevel.remote
	   // 0 = no verification, 1=remote (non-bootloader classes), 2=all classes
	   enum verifyLevel : Int { case none = 0, remote = 1, all = 2 }

	*/
}

// initialize the global values that are known at start-up
func initGlobals(progName string) *Globals {
	globals := new(Globals)
	globals.startTime = time.Now()
	globals.jacobinName = progName
	globals.version = "0.1.0"
	globals.logLevel = WARNING
	return globals
}
