package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blacktop/go-termimg"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"
	appPkg "github.com/steipete/wacli/internal/app"
	"github.com/steipete/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

type tuiMode int

const (
	tuiModeNormal tuiMode = iota
	tuiModeChatFilter
	tuiModeMessageSearch
	tuiModeSendText
	tuiModeSendFile
)

const (
	tuiRefreshInterval   = 2 * time.Second
	tuiNotificationLimit = 8
	tuiNotificationFlash = 2500 * time.Millisecond
)

type tuiNotification struct {
	ChatJID    string
	ChatName   string
	Timestamp  time.Time
	Text       string
	FlashUntil time.Time
}

type tuiState struct {
	app *tview.Application

	flags *rootFlags
	mode  tuiMode

	db *store.DB
	wa *appPkg.App

	chatFilter string
	msgQuery   string

	chats           []store.Chat
	messages        []store.Message
	messageRowIndex []int

	selectedChatJID string
	selectedMsgIdx  int
	followBottom    bool

	lastSeenChatTS     map[string]time.Time
	lastRenderedChatTS map[string]time.Time
	notifications      []tuiNotification
	lastReadChatTS     map[string]time.Time
	unreadCounts       map[string]int
	knownChats         map[string]bool
	startedAt          time.Time

	refreshInterval time.Duration
	skipChatChange  bool
	syncStatus      string
	appRunning      bool

	chatsView    *tview.List
	searchView   *tview.InputField
	messagesView *tview.List
	rightView    *tview.TextView
	syncView     *tview.TextView
	keysView     *tview.TextView
	inputView    *tview.InputField
	suggestView  *tview.TextView
	statusView   *tview.TextView

	focusedPane string
	statusNote  string

	suggestItems  []string
	suggestIndex  int
	suggestKey    string
	suggestActive bool
}

func newTuiCmd(flags *rootFlags) *cobra.Command {
	var noSync bool
	var syncDownloadMedia bool
	var syncRefreshContacts bool
	var syncRefreshGroups bool

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Read-only TUI (ranger-like layout)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			needLock := !noSync
			a, lk, err := newApp(ctx, flags, needLock, true, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			state := newTuiState(flags)
			state.setApp(a)
			state.setStore(a.DB())

			if !noSync {
				state.setSyncStatus("starting")
				go func() {
					if err := a.EnsureAuthed(); err != nil {
						state.setSyncStatus("unauth (run wacli auth)")
						return
					}
					state.setSyncStatus("running")
					_, err := a.Sync(ctx, appPkg.SyncOptions{
						Mode:            appPkg.SyncModeFollow,
						AllowQR:         false,
						DownloadMedia:   syncDownloadMedia,
						RefreshContacts: syncRefreshContacts,
						RefreshGroups:   syncRefreshGroups,
						Quiet:           true,
					})
					if err != nil {
						state.setSyncStatus(fmt.Sprintf("error: %v", err))
						return
					}
					state.setSyncStatus("stopped")
				}()
			}

			err = state.run(ctx)
			cancel()
			return err
		},
	}

	cmd.Flags().BoolVar(&noSync, "no-sync", false, "disable background sync")
	cmd.Flags().BoolVar(&syncDownloadMedia, "sync-download-media", false, "download media in the background during sync")
	cmd.Flags().BoolVar(&syncRefreshContacts, "sync-refresh-contacts", false, "refresh contacts from session store into local DB")
	cmd.Flags().BoolVar(&syncRefreshGroups, "sync-refresh-groups", false, "refresh joined groups (live) into local DB")
	return cmd
}

func newTuiState(flags *rootFlags) *tuiState {
	app := tview.NewApplication()

	applyTuiTheme()

	chats := tview.NewList().ShowSecondaryText(false)
	chats.SetBorder(true).SetTitle("whatsTUI")
	chats.SetTitleAlign(tview.AlignLeft)

	search := tview.NewInputField()
	search.SetFieldWidth(0)
	search.SetPlaceholder("Search chats (/)")
	search.SetBorder(true).SetTitle("Search").SetTitleAlign(tview.AlignLeft)

	messages := tview.NewList().ShowSecondaryText(false)
	messages.SetBorder(true).SetTitle("Messages")
	messages.SetTitleAlign(tview.AlignLeft)

	right := tview.NewTextView().SetDynamicColors(true)
	right.SetBorder(true).SetTitle("Info")

	syncView := tview.NewTextView().SetDynamicColors(true)
	syncView.SetBorder(true).SetTitle("Sync")
	syncView.SetTextAlign(tview.AlignCenter)

	keysView := tview.NewTextView().SetDynamicColors(true)
	keysView.SetBorder(true).SetTitle("KEY-BINDINGS").SetTitleAlign(tview.AlignLeft)

	input := tview.NewInputField()
	input.SetFieldWidth(0)

	suggest := tview.NewTextView().SetDynamicColors(true)
	suggest.SetTextAlign(tview.AlignLeft)
	suggest.SetBorder(false)

	status := tview.NewTextView().SetDynamicColors(true)
	status.SetTextAlign(tview.AlignLeft)

	state := &tuiState{
		app:                app,
		flags:              flags,
		mode:               tuiModeNormal,
		chatsView:          chats,
		searchView:         search,
		messagesView:       messages,
		rightView:          right,
		syncView:           syncView,
		keysView:           keysView,
		inputView:          input,
		suggestView:        suggest,
		statusView:         status,
		lastSeenChatTS:     map[string]time.Time{},
		lastRenderedChatTS: map[string]time.Time{},
		lastReadChatTS:     map[string]time.Time{},
		unreadCounts:       map[string]int{},
		knownChats:         map[string]bool{},
		startedAt:          time.Now().UTC(),
		refreshInterval:    tuiRefreshInterval,
		focusedPane:        "messages",
		followBottom:       true,
	}

	state.wireKeys()
	state.updateFocusBorders()

	center := tview.NewFlex().SetDirection(tview.FlexRow)
	center.AddItem(messages, 0, 1, true)

	inputBox := tview.NewFlex().SetDirection(tview.FlexRow)
	inputBox.AddItem(input, 1, 0, false)
	inputBox.AddItem(suggest, 4, 0, false)
	inputBox.AddItem(status, 1, 0, false)
	inputBox.SetBorder(true).SetTitle("Send").SetTitleAlign(tview.AlignLeft)

	center.AddItem(inputBox, 3, 0, false)

	leftCol := tview.NewFlex().SetDirection(tview.FlexRow)
	leftCol.AddItem(search, 3, 0, false)
	leftCol.AddItem(chats, 0, 1, true)

	rightCol := tview.NewFlex().SetDirection(tview.FlexRow)
	rightCol.AddItem(syncView, 3, 0, false)
	rightCol.AddItem(right, 0, 1, false)
	rightCol.AddItem(keysView, 11, 0, false)

	root := tview.NewFlex().
		AddItem(leftCol, 0, 1, true).
		AddItem(center, 0, 2, true).
		AddItem(rightCol, 0, 1, false)

	app.SetRoot(root, true)
	return state
}

func (s *tuiState) setStore(db *store.DB) {
	s.db = db
	if s.db != nil {
		s.reloadChats()
	}
}

func (s *tuiState) setApp(app *appPkg.App) {
	s.wa = app
}

func (s *tuiState) run(ctx context.Context) error {
	s.setFocus("messages")
	s.appRunning = true
	s.startRefreshLoop(ctx)
	s.updateStatus()
	return s.app.Run()
}

func (s *tuiState) wireKeys() {
	s.applyInputStyle()
	s.applySearchStyle()
	s.updateKeyBindings()

	s.chatsView.SetChangedFunc(func(i int, main, secondary string, shortcut rune) {
		if s.skipChatChange {
			return
		}
		_ = main
		_ = secondary
		_ = shortcut
		if i < 0 || i >= len(s.chats) {
			return
		}
		s.selectChat(s.chats[i].JID)
	})

	s.chatsView.SetSelectedFunc(func(i int, main, secondary string, shortcut rune) {
		_ = shortcut
		if i < 0 || i >= len(s.chats) {
			return
		}
		s.selectChat(s.chats[i].JID)
		s.setFocus("messages")
	})

	s.messagesView.SetSelectedFunc(func(i int, main, secondary string, shortcut rune) {
		_ = main
		_ = secondary
		_ = shortcut
		s.selectedMsgIdx = i
	})

	s.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if s.app.GetFocus() == s.searchView {
			switch event.Key() {
			case tcell.KeyEnter:
				idx := s.chatsView.GetCurrentItem()
				if idx >= 0 && idx < len(s.chats) {
					s.selectChat(s.chats[idx].JID)
				}
				s.setFocus("messages")
				return nil
			case tcell.KeyUp:
				s.moveSelection(-1)
				return nil
			case tcell.KeyDown:
				s.moveSelection(1)
				return nil
			case tcell.KeyEsc:
				s.searchView.SetText("")
				s.chatFilter = ""
				s.mode = tuiModeNormal
				s.reloadChats()
				s.updateStatus()
				s.setFocus("messages")
				return nil
			}
			return event
		}
		if s.app.GetFocus() == s.inputView {
			if event.Key() == tcell.KeyTab && s.mode == tuiModeSendFile {
				if s.autocompleteFilePath() {
					return nil
				}
			}
			if s.mode == tuiModeSendFile && s.suggestActive {
				switch event.Key() {
				case tcell.KeyUp:
					s.suggestIndex = clampIndex(s.suggestIndex-1, len(s.suggestItems))
					s.updateSuggestions()
					s.applySuggestionToInput()
					return nil
				case tcell.KeyDown:
					s.suggestIndex = clampIndex(s.suggestIndex+1, len(s.suggestItems))
					s.updateSuggestions()
					s.applySuggestionToInput()
					return nil
				}
			}
			if event.Key() == tcell.KeyEsc {
				s.mode = tuiModeNormal
				s.inputView.SetText("")
				s.inputView.SetLabel("")
				s.clearSuggestions()
				s.updateInputPlaceholder()
				s.updateStatus()
				s.setFocus("messages")
				return nil
			}
			return event
		}

		key := event.Key()
		switch key {
		case tcell.KeyEsc:
			s.mode = tuiModeNormal
			s.inputView.SetText("")
			s.inputView.SetLabel("")
			s.updateStatus()
			return nil
		}

		switch event.Rune() {
		case 'q':
			s.app.Stop()
			return nil
		case 'p':
			if s.app.GetFocus() == s.messagesView {
				if err := s.previewSelectedImage(); err != nil {
					s.flashStatus(fmt.Sprintf("preview failed: %v", err))
				}
				return nil
			}
		case '/':
			s.mode = tuiModeChatFilter
			s.setFocus("search")
			return nil
		case 'i':
			if s.app.GetFocus() == s.messagesView || s.app.GetFocus() == s.chatsView {
				s.mode = tuiModeSendText
				s.inputView.SetLabel("msg: ")
				s.inputView.SetText("")
				s.updateInputPlaceholder()
				s.setFocus("input")
				return nil
			}
		case 'f':
			if s.app.GetFocus() == s.messagesView || s.app.GetFocus() == s.chatsView {
				s.mode = tuiModeSendFile
				s.inputView.SetLabel("file: ")
				s.inputView.SetText("")
				s.updateInputPlaceholder()
				s.setFocus("input")
				return nil
			}
		case 'h':
			s.setFocus("chats")
			return nil
		case 'l':
			s.setFocus("messages")
			return nil
		case 'j':
			s.moveSelection(1)
			return nil
		case 'k':
			s.moveSelection(-1)
			return nil
		case 'g':
			s.jumpTop()
			return nil
		case 'G':
			s.jumpBottom()
			return nil
		case '?':
			s.mode = tuiModeMessageSearch
			s.inputView.SetLabel("search: ")
			s.inputView.SetText("")
			s.updateInputPlaceholder()
			s.setFocus("input")
			return nil
		}
		return event
	})

	s.searchView.SetChangedFunc(func(text string) {
		if s.mode != tuiModeChatFilter {
			return
		}
		s.chatFilter = strings.TrimSpace(text)
		s.reloadChats()
		s.updateStatus()
	})

	s.inputView.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(s.inputView.GetText())
		switch s.mode {
		case tuiModeMessageSearch:
			s.msgQuery = text
			s.reloadMessages(false)
		case tuiModeSendText:
			if err := s.sendTextMessage(text); err != nil {
				s.flashStatus(fmt.Sprintf("send failed: %v", err))
			} else {
				s.flashStatus("message sent")
			}
			s.reloadMessages(true)
		case tuiModeSendFile:
			if err := s.sendFileMessage(text); err != nil {
				s.flashStatus(fmt.Sprintf("send failed: %v", err))
			} else {
				s.flashStatus("file sent")
			}
			s.reloadMessages(true)
		}
		s.mode = tuiModeNormal
		s.inputView.SetText("")
		s.inputView.SetLabel("")
		s.clearSuggestions()
		s.updateInputPlaceholder()
		s.updateStatus()
		s.setFocus("messages")
	})
}

func (s *tuiState) moveSelection(delta int) {
	if s.app.GetFocus() == s.chatsView {
		idx := s.chatsView.GetCurrentItem()
		s.chatsView.SetCurrentItem(clampIndex(idx+delta, s.chatsView.GetItemCount()))
		return
	}
	if s.app.GetFocus() == s.messagesView {
		idx := s.messagesView.GetCurrentItem()
		next := clampIndex(idx+delta, s.messagesView.GetItemCount())
		s.messagesView.SetCurrentItem(next)
		if delta < 0 {
			s.followBottom = false
		} else if next >= s.messagesView.GetItemCount()-1 {
			s.followBottom = true
		}
		return
	}
}

func (s *tuiState) jumpTop() {
	if s.app.GetFocus() == s.chatsView {
		s.chatsView.SetCurrentItem(0)
		return
	}
	if s.app.GetFocus() == s.messagesView {
		s.messagesView.SetCurrentItem(0)
		s.followBottom = false
		return
	}
}

func (s *tuiState) jumpBottom() {
	if s.app.GetFocus() == s.chatsView {
		s.chatsView.SetCurrentItem(maxIndex(s.chatsView.GetItemCount()))
		return
	}
	if s.app.GetFocus() == s.messagesView {
		s.messagesView.SetCurrentItem(maxIndex(s.messagesView.GetItemCount()))
		s.followBottom = true
		return
	}
}

func clampIndex(idx, count int) int {
	if count <= 0 {
		return 0
	}
	if idx < 0 {
		return 0
	}
	if idx >= count {
		return count - 1
	}
	return idx
}

func maxIndex(count int) int {
	if count <= 0 {
		return 0
	}
	return count - 1
}

func lastMessageRow(rows []int) int {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] >= 0 {
			return i
		}
	}
	return -1
}

func (s *tuiState) reloadChats() {
	s.loadChats(false)
}

func (s *tuiState) startRefreshLoop(ctx context.Context) {
	if s.refreshInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.refreshInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.app.QueueUpdateDraw(func() {
					s.refreshFromStore()
				})
			}
		}
	}()
}

func (s *tuiState) refreshFromStore() {
	s.loadChats(true)
}

func (s *tuiState) loadChats(preserveSelection bool) {
	if s.db == nil {
		return
	}
	chats, err := s.db.ListChats(s.chatFilter, 200)
	if err != nil {
		s.statusView.SetText(fmt.Sprintf("error: %v", err))
		return
	}
	filtered := make([]store.Chat, 0, len(chats))
	prevSelected := s.selectedChatJID
	s.chatsView.Clear()
	for _, c := range chats {
		if shouldHideDMChat(c.JID, c.Kind) {
			continue
		}
		if !s.knownChats[c.JID] {
			s.knownChats[c.JID] = true
			if _, ok := s.lastReadChatTS[c.JID]; !ok {
				if !s.startedAt.IsZero() {
					s.lastReadChatTS[c.JID] = s.startedAt
				} else {
					s.lastReadChatTS[c.JID] = c.LastMessageTS
				}
			}
		}
		name := strings.TrimSpace(c.Name)
		if name == "" && c.Kind == "dm" && strings.HasSuffix(c.JID, "@s.whatsapp.net") {
			if contact, err := s.db.GetContact(c.JID); err == nil {
				name = strings.TrimSpace(contact.Name)
			}
		}
		if name == "" {
			if c.Kind == "dm" && strings.HasSuffix(c.JID, "@s.whatsapp.net") {
				continue
			}
			name = c.JID
		}
		count := 0
		if lastRead, ok := s.lastReadChatTS[c.JID]; ok && c.LastMessageTS.After(lastRead) {
			if n, err := s.db.CountMessagesAfter(c.JID, lastRead); err == nil {
				count = n
			}
		}
		s.unreadCounts[c.JID] = count
		label := fmt.Sprintf("%s  %s", truncate(name, 28), c.Kind)
		if count > 0 {
			label = fmt.Sprintf("[#00D4FF]%s[-]  [#00D4FF]+%d[-]", truncate(name, 28), count)
			label = fmt.Sprintf("%s  %s", label, c.Kind)
		}
		line := label
		s.chatsView.AddItem(line, "", 0, nil)
		filtered = append(filtered, c)
	}
	s.chats = filtered
	if len(filtered) == 0 {
		s.selectedChatJID = ""
		s.messagesView.Clear()
		s.updateRightPane()
		s.updateStatus()
		return
	}
	selectedIdx := 0
	if preserveSelection && strings.TrimSpace(prevSelected) != "" {
		if idx := chatIndexByJID(filtered, prevSelected); idx >= 0 {
			selectedIdx = idx
		} else {
			prevSelected = ""
		}
	}
	if !preserveSelection || strings.TrimSpace(prevSelected) == "" {
		selectedIdx = 0
	}
	s.skipChatChange = true
	s.chatsView.SetCurrentItem(selectedIdx)
	s.skipChatChange = false
	selectedJID := filtered[selectedIdx].JID
	if selectedJID != s.selectedChatJID {
		s.selectedChatJID = selectedJID
		s.reloadMessages(false)
	} else if preserveSelection {
		s.maybeReloadMessages(filtered[selectedIdx])
	}
	s.detectNotifications(filtered)
	s.updateMessagesTitle()
	s.updateRightPane()
	s.updateStatus()
}

func (s *tuiState) selectChat(jid string) {
	if strings.TrimSpace(jid) == "" {
		return
	}
	s.selectedChatJID = jid
	if s.db != nil {
		if chat, err := s.db.GetChat(jid); err == nil {
			s.lastReadChatTS[jid] = chat.LastMessageTS
			s.unreadCounts[jid] = 0
		}
	}
	s.updateMessagesTitle()
	s.reloadMessages(false)
	s.updateRightPane()
}

func (s *tuiState) reloadMessages(keepSelection bool) {
	if s.db == nil {
		return
	}
	if s.selectedChatJID == "" {
		s.messagesView.Clear()
		return
	}
	prevIdx := s.messagesView.GetCurrentItem()
	prevCount := s.messagesView.GetItemCount()
	wasAtBottom := prevCount > 0 && prevIdx >= prevCount-1
	var msgs []store.Message
	var err error
	if strings.TrimSpace(s.msgQuery) != "" {
		msgs, err = s.db.SearchMessages(store.SearchMessagesParams{
			Query:   s.msgQuery,
			ChatJID: s.selectedChatJID,
			Limit:   200,
		})
	} else {
		msgs, err = s.db.ListMessages(store.ListMessagesParams{
			ChatJID: s.selectedChatJID,
			Limit:   200,
		})
	}
	if err != nil {
		s.statusView.SetText(fmt.Sprintf("error: %v", err))
		return
	}
	// DB returns newest first; render oldest -> newest.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	s.messages = msgs
	s.messagesView.Clear()
	s.messageRowIndex = s.messageRowIndex[:0]
	_, _, width, _ := s.messagesView.GetInnerRect()
	if width <= 0 {
		width = 80
	}
	for msgIdx, m := range msgs {
		from := m.SenderJID
		if m.FromMe {
			from = "me"
		}
		text := strings.TrimSpace(m.DisplayText)
		if text == "" {
			text = strings.TrimSpace(m.Text)
		}
		if strings.EqualFold(m.MediaType, "image") {
			local := strings.TrimSpace(m.LocalPath)
			if local != "" {
				text = "image | local: " + local
			} else {
				text = "image | press p to preview (auto-download)"
			}
		} else if m.MediaType != "" && text == "" {
			text = "Sent " + m.MediaType
		}
		ts := m.Timestamp.Local().Format("15:04")
		if m.FromMe {
			lines := wrapText(text, width-8)
			for i, ln := range lines {
				out := ln
				if i == 0 {
					out = fmt.Sprintf("%s  %s", ln, ts)
				}
				s.messagesView.AddItem(padLeft(out, width), "", 0, nil)
				s.messageRowIndex = append(s.messageRowIndex, msgIdx)
			}
		} else {
			fromLabel := truncate(from, 12)
			prefix := fmt.Sprintf("%s  %s: ", ts, fromLabel)
			lines := wrapText(text, max(1, width-len(prefix)))
			for i, ln := range lines {
				line := ln
				if i == 0 {
					line = prefix + ln
				}
				line = truncate(line, width)
				line = fmt.Sprintf("[#cba86a:#1e2124]%s[-:-:-]", line)
				s.messagesView.AddItem(line, "", 0, nil)
				s.messageRowIndex = append(s.messageRowIndex, msgIdx)
			}
		}
		s.messagesView.AddItem("", "", 0, nil)
		s.messageRowIndex = append(s.messageRowIndex, -1)
	}
	itemCount := s.messagesView.GetItemCount()
	lastMsgRow := lastMessageRow(s.messageRowIndex)
	if keepSelection {
		if s.followBottom || wasAtBottom {
			if lastMsgRow >= 0 {
				s.messagesView.SetCurrentItem(lastMsgRow)
			} else {
				s.messagesView.SetCurrentItem(maxIndex(itemCount))
			}
		} else {
			s.messagesView.SetCurrentItem(clampIndex(prevIdx, itemCount))
		}
	} else {
		s.followBottom = true
		if lastMsgRow >= 0 {
			s.messagesView.SetCurrentItem(lastMsgRow)
		} else {
			s.messagesView.SetCurrentItem(maxIndex(itemCount))
		}
	}
	if len(msgs) > 0 {
		s.lastRenderedChatTS[s.selectedChatJID] = msgs[len(msgs)-1].Timestamp
	} else {
		s.lastRenderedChatTS[s.selectedChatJID] = time.Time{}
	}
	s.updateStatus()
}

func (s *tuiState) maybeReloadMessages(chat store.Chat) {
	lastRendered := s.lastRenderedChatTS[chat.JID]
	if chat.LastMessageTS.After(lastRendered) {
		s.reloadMessages(true)
	}
}

func (s *tuiState) updateRightPane() {
	if s.db == nil || s.selectedChatJID == "" {
		s.rightView.SetText("")
		s.updateSyncPane()
		return
	}
	chat, err := s.db.GetChat(s.selectedChatJID)
	if err != nil {
		s.rightView.SetText(fmt.Sprintf("error: %v", err))
		s.updateSyncPane()
		return
	}
	s.rightView.Clear()
	s.updateSyncPane()
	name := chat.Name
	if strings.TrimSpace(name) == "" {
		name = chat.JID
	}
	last := ""
	if !chat.LastMessageTS.IsZero() {
		last = chat.LastMessageTS.Local().Format(time.RFC3339)
	}
	fmt.Fprintf(s.rightView, "Name: %s\nJID: %s\nKind: %s\nLast: %s\n", name, chat.JID, chat.Kind, last)
	fmt.Fprintf(s.rightView, "\nParticipants: (v1)\nMedia stats: (v1)\nTags: (v1)\n")
	fmt.Fprintf(s.rightView, "\n[#00D48A]NOTIFICATIONS[-]\n")
	if len(s.notifications) == 0 {
		fmt.Fprintf(s.rightView, "(none)\n")
		return
	}
	for i := len(s.notifications) - 1; i >= 0; i-- {
		n := s.notifications[i]
		ts := n.Timestamp.Local().Format("15:04:05")
		label := n.ChatName
		if strings.TrimSpace(label) == "" {
			label = n.ChatJID
		}
		line := fmt.Sprintf("[#cba86a]✦[-] %s  %s  %s", ts, truncate(label, 18), truncate(n.Text, 60))
		if time.Now().Before(n.FlashUntil) {
			fmt.Fprintf(s.rightView, "[black:#f5a742]%s[-:-:-]\n", line)
		} else {
			fmt.Fprintf(s.rightView, "%s\n", line)
		}
	}
}

func (s *tuiState) updateMessagesTitle() {
	if s.messagesView == nil {
		return
	}
	title := "Message"
	if strings.TrimSpace(s.selectedChatJID) != "" {
		for _, c := range s.chats {
			if c.JID == s.selectedChatJID {
				name := strings.TrimSpace(c.Name)
				if name == "" {
					name = c.JID
				}
				title = fmt.Sprintf("Message - %s", truncate(name, 28))
				break
			}
		}
	}
	s.messagesView.SetTitle(title)
	s.messagesView.SetTitleAlign(tview.AlignLeft)
}

func (s *tuiState) detectNotifications(chats []store.Chat) {
	for _, c := range chats {
		if c.LastMessageTS.IsZero() {
			continue
		}
		lastSeen, ok := s.lastSeenChatTS[c.JID]
		if !ok {
			s.lastSeenChatTS[c.JID] = c.LastMessageTS
			continue
		}
		if !c.LastMessageTS.After(lastSeen) {
			continue
		}
		s.lastSeenChatTS[c.JID] = c.LastMessageTS
		msgs, err := s.db.ListMessages(store.ListMessagesParams{
			ChatJID: c.JID,
			Limit:   1,
		})
		if err != nil || len(msgs) == 0 {
			continue
		}
		m := msgs[0]
		if m.FromMe {
			continue
		}
		msgText := s.messageText(m)
		name := c.Name
		if strings.TrimSpace(name) == "" {
			name = c.JID
		}
		s.pushNotification(tuiNotification{
			ChatJID:    c.JID,
			ChatName:   name,
			Timestamp:  c.LastMessageTS,
			Text:       msgText,
			FlashUntil: time.Now().Add(tuiNotificationFlash),
		})
	}
}

func (s *tuiState) latestMessageText(chatJID string) string {
	msgs, err := s.db.ListMessages(store.ListMessagesParams{
		ChatJID: chatJID,
		Limit:   1,
	})
	if err != nil || len(msgs) == 0 {
		return "(message)"
	}
	return s.messageText(msgs[0])
}

func (s *tuiState) messageText(m store.Message) string {
	text := strings.TrimSpace(m.DisplayText)
	if text == "" {
		text = strings.TrimSpace(m.Text)
	}
	if m.MediaType != "" && text == "" {
		text = "Sent " + m.MediaType
	}
	if text == "" {
		text = "(message)"
	}
	return text
}

func (s *tuiState) selectedMessage() (store.Message, bool) {
	if len(s.messages) == 0 || len(s.messageRowIndex) == 0 {
		return store.Message{}, false
	}
	row := s.messagesView.GetCurrentItem()
	if row < 0 || row >= len(s.messageRowIndex) {
		return store.Message{}, false
	}
	idx := s.messageRowIndex[row]
	if idx < 0 {
		for i := row - 1; i >= 0; i-- {
			if s.messageRowIndex[i] >= 0 {
				idx = s.messageRowIndex[i]
				break
			}
		}
	}
	if idx < 0 || idx >= len(s.messages) {
		return store.Message{}, false
	}
	return s.messages[idx], true
}

func (s *tuiState) previewSelectedImage() error {
	msg, ok := s.selectedMessage()
	if !ok {
		return fmt.Errorf("no message selected")
	}
	if !strings.EqualFold(msg.MediaType, "image") {
		return fmt.Errorf("selected message is not an image")
	}
	path := strings.TrimSpace(msg.LocalPath)
	if path == "" {
		ctx, cancel := withTimeout(context.Background(), s.flags)
		defer cancel()
		var err error
		path, err = s.downloadImage(ctx, msg)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}

	img, err := termimg.Open(path)
	if err != nil {
		return err
	}
	_ = s.app.Suspend(func() {
		_ = img.Scale(termimg.ScaleFit).Print()
		fmt.Fprint(os.Stdout, "\nPress Enter to return...")
		_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
		fmt.Fprint(os.Stdout, "\n")
	})
	return nil
}

func (s *tuiState) downloadImage(ctx context.Context, msg store.Message) (string, error) {
	if s.wa == nil {
		return "", fmt.Errorf("app not ready")
	}
	if err := s.wa.EnsureAuthed(); err != nil {
		return "", err
	}
	if !s.wa.WA().IsConnected() {
		if err := s.wa.Connect(ctx, false, nil); err != nil {
			return "", err
		}
	}
	info, err := s.wa.DB().GetMediaDownloadInfo(msg.ChatJID, msg.MsgID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(info.DirectPath) == "" || len(info.MediaKey) == 0 {
		return "", fmt.Errorf("no media metadata")
	}
	targetPath, err := s.wa.ResolveMediaOutputPath(info, "")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
		return "", err
	}
	if _, err := s.wa.WA().DownloadMediaToFile(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType, "", targetPath); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if err := s.wa.DB().MarkMediaDownloaded(info.ChatJID, info.MsgID, targetPath, now); err != nil {
		return "", err
	}
	return targetPath, nil
}

func (s *tuiState) pushNotification(n tuiNotification) {
	s.notifications = append(s.notifications, n)
	if len(s.notifications) <= tuiNotificationLimit {
		return
	}
	over := len(s.notifications) - tuiNotificationLimit
	if over > 0 {
		s.notifications = append([]tuiNotification{}, s.notifications[over:]...)
	}
}

func chatIndexByJID(chats []store.Chat, jid string) int {
	for i, c := range chats {
		if c.JID == jid {
			return i
		}
	}
	return -1
}

func shouldHideDMChat(jid, kind string) bool {
	if kind != "dm" {
		return false
	}
	jid = strings.TrimSpace(jid)
	if !strings.HasSuffix(jid, "@s.whatsapp.net") {
		return false
	}
	user := strings.TrimSuffix(jid, "@s.whatsapp.net")
	if user == "" {
		return false
	}
	if len(user) < 13 {
		return false
	}
	for _, r := range user {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (s *tuiState) setFocus(pane string) {
	switch pane {
	case "chats":
		s.app.SetFocus(s.chatsView)
	case "messages":
		s.app.SetFocus(s.messagesView)
	case "input":
		s.app.SetFocus(s.inputView)
	case "search":
		s.app.SetFocus(s.searchView)
	case "right":
		s.app.SetFocus(s.rightView)
	}
	if s.focusedPane != pane {
		s.focusedPane = pane
		s.updateFocusBorders()
	}
}

func (s *tuiState) updateFocusBorders() {
	focus := tcell.NewRGBColor(245, 167, 66) // amber
	normal := tcell.NewRGBColor(76, 86, 98)  // muted gray

	apply := func(pane string, box *tview.Box) {
		if pane == s.focusedPane {
			box.SetBorderColor(focus)
			box.SetTitleColor(focus)
		} else {
			box.SetBorderColor(normal)
			box.SetTitleColor(normal)
		}
	}

	apply("chats", s.chatsView.Box)
	apply("messages", s.messagesView.Box)
	apply("right", s.rightView.Box)
	apply("input", s.inputView.Box)
	apply("input", s.statusView.Box)
}

func applyTuiTheme() {
	styles := tview.Styles
	styles.PrimitiveBackgroundColor = tcell.NewRGBColor(20, 22, 24)
	styles.ContrastBackgroundColor = tcell.NewRGBColor(30, 33, 36)
	styles.MoreContrastBackgroundColor = tcell.NewRGBColor(44, 48, 54)
	styles.BorderColor = tcell.NewRGBColor(76, 86, 98)
	styles.TitleColor = tcell.NewRGBColor(203, 168, 106)
	styles.GraphicsColor = tcell.NewRGBColor(76, 86, 98)
	styles.PrimaryTextColor = tcell.NewRGBColor(214, 217, 220)
	styles.SecondaryTextColor = tcell.NewRGBColor(160, 170, 180)
	styles.TertiaryTextColor = tcell.NewRGBColor(120, 130, 140)
	styles.InverseTextColor = tcell.NewRGBColor(20, 22, 24)
	styles.ContrastSecondaryTextColor = tcell.NewRGBColor(203, 168, 106)
	tview.Styles = styles
}

func (s *tuiState) updateStatus() {
	chatLabel := s.selectedChatJID
	for _, c := range s.chats {
		if c.JID == s.selectedChatJID {
			if strings.TrimSpace(c.Name) != "" {
				chatLabel = c.Name
			}
			break
		}
	}
	mode := "normal"
	switch s.mode {
	case tuiModeChatFilter:
		mode = "filter"
	case tuiModeMessageSearch:
		mode = "search"
	case tuiModeSendText:
		mode = "send"
	case tuiModeSendFile:
		mode = "file"
	}
	status := fmt.Sprintf("chat: %s | msgs: %d | mode: %s", truncate(chatLabel, 24), len(s.messages), mode)
	if s.mode == tuiModeChatFilter && strings.TrimSpace(s.chatFilter) != "" {
		status = fmt.Sprintf("%s | filter: %s", status, truncate(s.chatFilter, 18))
	}
	if s.mode == tuiModeMessageSearch && strings.TrimSpace(s.msgQuery) != "" {
		status = fmt.Sprintf("%s | search: %s", status, truncate(s.msgQuery, 18))
	}
	if strings.TrimSpace(s.statusNote) != "" {
		status = fmt.Sprintf("%s | %s", status, truncate(s.statusNote, 40))
	}
	s.statusView.SetText(status)
}

func (s *tuiState) applyInputStyle() {
	s.inputView.SetFieldBackgroundColor(tcell.NewRGBColor(24, 26, 29))
	s.inputView.SetFieldTextColor(tcell.NewRGBColor(214, 217, 220))
	s.inputView.SetLabelColor(tcell.NewRGBColor(203, 168, 106))
	s.inputView.SetPlaceholderTextColor(tcell.NewRGBColor(120, 130, 140))
	s.suggestView.SetTextColor(tcell.NewRGBColor(160, 170, 180))
	s.updateInputPlaceholder()
}

func (s *tuiState) updateInputPlaceholder() {
	switch s.mode {
	case tuiModeSendText:
		s.inputView.SetPlaceholder("Type message and press Enter")
	case tuiModeSendFile:
		s.inputView.SetPlaceholder("Path to file (TAB to autocomplete)")
	default:
		s.inputView.SetPlaceholder("i: text, f: file")
	}
}

func (s *tuiState) applySearchStyle() {
	s.searchView.SetFieldBackgroundColor(tcell.NewRGBColor(24, 26, 29))
	s.searchView.SetFieldTextColor(tcell.NewRGBColor(214, 217, 220))
	s.searchView.SetLabelColor(tcell.NewRGBColor(0, 200, 255))
	s.searchView.SetPlaceholderTextColor(tcell.NewRGBColor(120, 130, 140))
}

func (s *tuiState) updateKeyBindings() {
	if s.keysView == nil {
		return
	}
	s.keysView.SetTitleColor(tcell.NewRGBColor(0, 200, 255))
	lines := []string{
		"[#cba86a]i[-]  send text",
		"[#cba86a]f[-]  send file",
		"[#cba86a]p[-]  preview image",
		"[#cba86a]/[-]  search chats",
		"[#cba86a]?[-]  search messages",
		"[#cba86a]↑/↓[-] navigate (search/suggest)",
		"[#cba86a]Tab[-] autocomplete file path",
		"[#cba86a]j/k[-] move up/down",
		"[#cba86a]h/l[-] move left/right",
		"[#cba86a]g/G[-] top/bottom",
		"[#cba86a]Esc[-] cancel input",
		"[#cba86a]q[-]  quit",
	}
	s.keysView.SetText(strings.Join(lines, "\n"))
}

func (s *tuiState) flashStatus(msg string) {
	s.statusNote = msg
	s.updateStatus()
	time.AfterFunc(3*time.Second, func() {
		s.app.QueueUpdateDraw(func() {
			if s.statusNote == msg {
				s.statusNote = ""
				s.updateStatus()
			}
		})
	})
}

func (s *tuiState) sendTextMessage(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("message is empty")
	}
	if s.wa == nil {
		return fmt.Errorf("app not ready")
	}
	if strings.TrimSpace(s.selectedChatJID) == "" {
		return fmt.Errorf("no chat selected")
	}
	jid, err := types.ParseJID(s.selectedChatJID)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(context.Background(), s.flags)
	defer cancel()

	if err := s.wa.EnsureAuthed(); err != nil {
		return err
	}
	if !s.wa.WA().IsConnected() {
		if err := s.wa.Connect(ctx, false, nil); err != nil {
			return err
		}
	}
	msgID, err := s.wa.WA().SendText(ctx, jid, text)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	chatName := s.wa.WA().ResolveChatName(ctx, jid, "")
	kind := chatKindFromJID(jid)
	_ = s.wa.DB().UpsertChat(jid.String(), kind, chatName, now)
	_ = s.wa.DB().UpsertMessage(store.UpsertMessageParams{
		ChatJID:    jid.String(),
		ChatName:   chatName,
		MsgID:      string(msgID),
		SenderJID:  "",
		SenderName: "me",
		Timestamp:  now,
		FromMe:     true,
		Text:       text,
	})
	return nil
}

func (s *tuiState) sendFileMessage(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("file path is empty")
	}
	if s.wa == nil {
		return fmt.Errorf("app not ready")
	}
	if strings.TrimSpace(s.selectedChatJID) == "" {
		return fmt.Errorf("no chat selected")
	}
	fullPath, err := expandUserPath(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory")
	}
	jid, err := types.ParseJID(s.selectedChatJID)
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(context.Background(), s.flags)
	defer cancel()
	if err := s.wa.EnsureAuthed(); err != nil {
		return err
	}
	if !s.wa.WA().IsConnected() {
		if err := s.wa.Connect(ctx, false, nil); err != nil {
			return err
		}
	}
	_, _, err = sendFile(ctx, s.wa, jid, fullPath, "", "", "")
	return err
}

func (s *tuiState) autocompleteFilePath() bool {
	raw := s.inputView.GetText()
	if raw == "" {
		s.inputView.SetText("./")
		return true
	}
	path, err := expandUserPath(raw)
	if err != nil {
		return false
	}
	dir := path
	base := ""
	if !strings.HasSuffix(path, string(os.PathSeparator)) {
		dir = filepath.Dir(path)
		base = filepath.Base(path)
	}
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, base) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		s.clearSuggestions()
		return false
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		next := filepath.Join(dir, matches[0])
		if info, err := os.Stat(next); err == nil && info.IsDir() {
			next += string(os.PathSeparator)
		}
		s.inputView.SetText(next)
		s.clearSuggestions()
		return true
	}
	key := fmt.Sprintf("%s|%s|%s", dir, base, strings.Join(matches, ","))
	if key == s.suggestKey {
		s.suggestIndex = (s.suggestIndex + 1) % len(matches)
	} else {
		s.suggestIndex = 0
	}
	s.suggestKey = key
	s.suggestItems = matches
	s.suggestActive = true
	s.updateSuggestions()

	next := filepath.Join(dir, matches[s.suggestIndex])
	if info, err := os.Stat(next); err == nil && info.IsDir() {
		next += string(os.PathSeparator)
	}
	s.inputView.SetText(next)
	return true
}

func expandUserPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:]), nil
		}
	}
	return p, nil
}

func commonPrefix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	prefix := items[0]
	for _, s := range items[1:] {
		for !strings.HasPrefix(s, prefix) && prefix != "" {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *tuiState) applySuggestionToInput() {
	if !s.suggestActive || len(s.suggestItems) == 0 {
		return
	}
	raw := s.inputView.GetText()
	path, err := expandUserPath(raw)
	if err != nil {
		return
	}
	dir := path
	if !strings.HasSuffix(path, string(os.PathSeparator)) {
		dir = filepath.Dir(path)
	}
	if dir == "" {
		dir = "."
	}
	next := filepath.Join(dir, s.suggestItems[s.suggestIndex])
	if info, err := os.Stat(next); err == nil && info.IsDir() {
		next += string(os.PathSeparator)
	}
	s.inputView.SetText(next)
}

func (s *tuiState) updateSuggestions() {
	if s.suggestView == nil {
		return
	}
	if !s.suggestActive || len(s.suggestItems) == 0 {
		s.suggestView.SetText("")
		return
	}
	var b strings.Builder
	max := min(6, len(s.suggestItems))
	start := 0
	if s.suggestIndex >= max {
		start = s.suggestIndex - max + 1
	}
	end := min(len(s.suggestItems), start+max)
	for i := start; i < end; i++ {
		item := s.suggestItems[i]
		prefix := "  "
		if i == s.suggestIndex {
			prefix = "[#cba86a]»[-] "
		}
		fmt.Fprintf(&b, "%s%s\n", prefix, item)
	}
	s.suggestView.SetText(strings.TrimRight(b.String(), "\n"))
}

func (s *tuiState) clearSuggestions() {
	s.suggestItems = nil
	s.suggestIndex = 0
	s.suggestKey = ""
	s.suggestActive = false
	if s.suggestView != nil {
		s.suggestView.SetText("")
	}
}

func (s *tuiState) setSyncStatus(status string) {
	s.syncStatus = status
	if s.app == nil || !s.appRunning {
		return
	}
	s.app.QueueUpdateDraw(func() {
		s.updateSyncPane()
		s.updateStatus()
	})
}

func (s *tuiState) syncIndicatorLine() string {
	status := strings.TrimSpace(s.syncStatus)
	if status == "" {
		return ""
	}
	dot := "[yellow]●[-]"
	lower := strings.ToLower(status)
	switch {
	case strings.Contains(lower, "running"):
		dot = "[green]●[-]"
	case strings.Contains(lower, "unauth"), strings.Contains(lower, "error"), strings.Contains(lower, "stopped"):
		dot = "[red]●[-]"
	}
	return fmt.Sprintf("%s %s", dot, status)
}

func (s *tuiState) updateSyncPane() {
	if s.syncView == nil {
		return
	}
	line := s.syncIndicatorLine()
	if line == "" {
		s.syncView.SetText("[red]●[-] off")
		return
	}
	s.syncView.SetText(line)
}

func padLeft(s string, width int) string {
	if width <= 0 {
		return s
	}
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

func wrapText(s string, width int) []string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if width <= 0 {
		return []string{s}
	}
	if s == "" {
		return []string{""}
	}
	var lines []string
	for len(s) > 0 {
		if len(s) <= width {
			lines = append(lines, s)
			break
		}
		cut := strings.LastIndexAny(s[:width+1], " \t")
		if cut <= 0 {
			cut = width
		}
		lines = append(lines, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
