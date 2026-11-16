package media

// uprightRotation is Camera2's own rule (as for JPEG_ORIENTATION): the
// clockwise turn that makes a frame upright. Front cameras turn the other
// way, since they face the user; their frames aren't mirrored, so what's
// sent reads the right way round.
func uprightRotation(sensor, device int, front bool) int {
	if front {
		device = -device
	}
	return ((sensor+device)%360 + 360) % 360
}

// quarterTurn rounds degrees to the nearest of 0, 90, 180, 270.
func quarterTurn(deg int) int {
	deg = ((deg % 360) + 360) % 360
	return ((deg + 45) / 90 * 90) % 360
}
