package yggdrasil

func getDefaults() tunDefaultParameters {
	return tunDefaultParameters{
		maximumIfMTU:     16384,
		defaultIfMTU:     16384,
		defaultIfName:    "/dev/tap0",
		defaultIfTAPMode: true,
	}
}
