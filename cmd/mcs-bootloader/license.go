package main

import (
	"errors"
	"fmt"
	"io"

	assets "github.com/difof/mcs-bootloader"
)

func printLicense(o cliOptions, out *cliOutput) error {
	if len(o.Args) != 0 {
		return errors.New("license takes no arguments")
	}
	if out.JSON {
		return writeJSON(out.Out, map[string]string{"license": assets.LicenseID, "text": assets.License})
	}
	if _, err := fmt.Fprintln(out.Out, out.color(out.Out, "1;36", "mcs-bootloader — "+assets.LicenseID)); err != nil {
		return err
	}
	_, err := io.WriteString(out.Out, assets.License)
	return err
}
