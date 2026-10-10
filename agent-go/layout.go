package main

// v8.2: Horizontal resize support — repositions controls when the window
// is widened so the layout stretches instead of leaving empty space.

var pMoveWindow = u32.NewProc("MoveWindow")

func moveCtl(id int, x, y, w, h int32) {
	if h := gCtl[id]; h != 0 {
		pMoveWindow.Call(h, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 1)
	}
}

func layoutControls(clientW int32) {
	// clientW is the full client width; controls use 28px margins
	right := clientW - 28
	// full-width buttons stretch
	fullW := clientW - 56
	moveCtl(ctlUpdate, 28, 548, fullW, 34)
	moveCtl(ctlQuit, 28, 634, fullW, 32)
	// right-aligned copy buttons move to the right edge
	moveCtl(ctlCopyID, right-96, 138, 96, 32)
	moveCtl(ctlNewPIN, right-100, 200, 100, 30)
	moveCtl(ctlCopy, right-96, 288, 96, 32)
	moveCtl(ctlCopyClaim, right-100, 346, 100, 30)
	// text fields stretch
	moveCtl(ctlIDValue, 28, 136, right-96-28-8, 36)
	moveCtl(ctlPINValue, 28, 202, right-100-28-8, 30)
	moveCtl(ctlLinkEdit, 28, 290, right-96-28-8, 28)
	moveCtl(ctlClaimVal, 28, 348, right-100-28-8, 30)
	moveCtl(ctlWakeVal, 28, 518, fullW, 18)
}
