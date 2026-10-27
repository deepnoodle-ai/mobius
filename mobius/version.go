package mobius

import "runtime/debug"

const modulePath = "github.com/deepnoodle-ai/mobius"

// sdkVersion returns the version of this module that the running binary was
// built with, for the default X-Mobius-Client value. Go records it in the
// binary's build info: as a dependency for a program that imports the SDK,
// or as the main module for the mobius CLI. A local-checkout build records
// "(devel)", which reports as "dev".
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return moduleVersion(info)
}

func moduleVersion(info *debug.BuildInfo) string {
	version := ""
	if info.Main.Path == modulePath {
		version = info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			version = dep.Version
			if dep.Replace != nil && dep.Replace.Version != "" {
				version = dep.Replace.Version
			}
		}
	}
	if version == "" || version == "(devel)" {
		return "dev"
	}
	return version
}
