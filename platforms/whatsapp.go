package platforms

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"mime"
	"os"
	"time"

	"github.com/adlio/trello"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	store "github.com/drdeee/whatsapp-trello-bridge/store"
)

type WhatsAppClient struct {
	Client       *whatsmeow.Client
	trelloClient *TrelloClient
	store        *store.RequestStore
	ready        bool
	infoChat     string
}

func (c *WhatsAppClient) Init(trelloClient *TrelloClient, store *store.RequestStore) {
	_, err := os.Stat("./tmp")
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("Creating temp folder")
		os.Mkdir("./tmp", 0777)
	}

	c.trelloClient = trelloClient
	c.store = store
	fmt.Println("Initializing WhatsApp client")
	dbLog := waLog.Stdout("Database", "WARN", true)
	container, err := sqlstore.New("sqlite3", "file:"+os.Getenv("WHATSAPP_DATABASE_FILE")+"?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		panic(err)
	}
	clientLog := waLog.Stdout("WhatsApp Client", "WARN", true)
	c.Client = whatsmeow.NewClient(deviceStore, clientLog)
	c.Client.AddEventHandler(c.eventHandler)

	if c.Client.Store.ID == nil {
		qrChan, _ := c.Client.GetQRChannel(context.Background())
		err = c.Client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				fmt.Println("Login event:", evt.Event)
			}
		}
	} else {
		// Already logged in, just connect
		err = c.Client.Connect()
		if err != nil {
			panic(err)
		}
		c.ready = true
	}

	if os.Getenv("INFO_ROOM") != "" {
		c.infoChat = os.Getenv("INFO_ROOM")
		jid, err := types.ParseJID(c.infoChat)
		if err != nil {
			c.infoChat = ""
		}
		info, err := c.Client.GetGroupInfo(jid)
		if err != nil {
			c.infoChat = ""
		} else {
			c.infoChat = info.JID.ToNonAD().String()
		}
	}

	if c.infoChat == "" {
		// print all joined groups
		groups, err := c.Client.GetJoinedGroups()
		if err != nil {
			fmt.Println("Failed to get joined groups:", err)
		} else {
			fmt.Println("=== Joined groups ===")
			for _, g := range groups {
				fmt.Println(g.Name, g.JID.ToNonAD().String())
			}
		}

	}
}

func (c *WhatsAppClient) IsReady() bool {
	return c.ready
}

func (c *WhatsAppClient) eventHandler(event interface{}) {
	switch evt := event.(type) {
	case *events.Message:
		if evt.Info.IsGroup {
			return
		}

		if evt.Info.IsFromMe {
			return
		}

		c.Client.MarkRead([]string{evt.Info.ID}, time.Now(), evt.Info.Chat, evt.Info.Sender)
		hasAttachment, attachmentFile, attachmentName, msgContent, err := c.getAttachment(evt)
		if msgContent == "" {
			if evt.Message.GetConversation() != "" {
				msgContent = evt.Message.GetConversation()
			} else if extended := evt.Message.GetExtendedTextMessage(); extended != nil {
				msgContent = extended.GetText()
			}
		}
		if err != nil {
			if err.Error() != "type unsupported" {
				fmt.Println(err)
				c.SendText(*evt, "Der Anhang deiner Nachricht konnte nicht heruntergeladen werden :(")
			}
			return
		}
		state, err := c.store.GetState(evt.Info.Sender.ToNonAD().String())
		if err != nil || state == "" {
			var card = &trello.Card{
				Name:    c.getUsername(evt),
				Desc:    msgContent,
				IDBoard: os.Getenv("TRELLO_BOARD_ID"),
				IDList:  c.trelloClient.Lists.New}
			err := c.trelloClient.Client.CreateCard(card)
			if err == nil {
				err = c.trelloClient.SetTrelloCustomFieldValue(card.ID, evt.Info.Sender.ToNonAD().String())
				if err == nil && hasAttachment {
					err = c.trelloClient.UploadTrelloAttachment(card.ID, attachmentFile, attachmentName)
				}
			}
			if err != nil {
				fmt.Println("Error creating card:", err)
				c.SendInfoMessage("*Fehler beim Erstellen eines Tickets :(\n\nhttps://wa.me/" + evt.Info.Chat.User)
				c.SendText(*evt, "Deine Anfrage konnte nicht weitergeleitet werden :( Bitte versuche es später nochmal erneut.")
			} else {
				c.store.SetState(evt.Info.Sender.ToNonAD().String(), card.ID)
				c.SendInfoMessage("Neues Ticket von @" + evt.Info.Chat.User + "\n\nhttps://trello.com/c/" + card.ShortLink)
				c.SendText(*evt, "Deine Anfrage wurde erfolgreich weitergeleitet. Wir kümmern uns so schnell wie möglich darum.")
			}

		} else {
			card, err := c.trelloClient.Client.GetCard(state)
			if err != nil {
				fmt.Println("Error adding comment to card:", err)
				c.SendInfoMessage("*Fehler beim Weiterleiten einer Nachricht*\n\nhttps://wa.me/" + evt.Info.Chat.User)
				c.SendText(*evt, "Deine Nachricht konnte nicht weitergeleitet werden :( Bitte versuche es später nochmal erneut.")
			} else {
				msg := "**[USER]** " + msgContent
				if hasAttachment {
					msg += msgContent + "\n\n*(Neuer Anhang)* "
				}
				comment, err := card.AddComment(msg)
				if err == nil && hasAttachment {
					err = c.trelloClient.UploadTrelloAttachment(card.ID, attachmentFile, attachmentName)
				}
				if err != nil {
					fmt.Println("Error adding comment to card:", err)
					c.SendInfoMessage("*Fehler beim Weiterleiten einer Nachricht*\n\nhttps://wa.me/" + evt.Info.Chat.User)
					c.SendText(*evt, "Deine Nachricht konnte nicht weitergeleitet werden :( Bitte versuche es später nochmal erneut.")
				} else {
					c.SendInfoMessage("Neue Nachricht von @" + evt.Info.Chat.User + "\n\nhttps://trello.com/c/" + comment.Data.Card.ShortLink)
					c.SendText(*evt, "Deine Nachricht wurde deiner Anfrage hinzugefügt.")
				}
			}
		}
	}
}

func (c *WhatsAppClient) getUsername(evt *events.Message) string {
	number := evt.Info.Sender.User
	contact, err := c.Client.Store.Contacts.GetContact(evt.Info.Sender)
	if err != nil || !contact.Found {
		if evt.Info.PushName != "" {
			return evt.Info.PushName + " (" + number + ")"
		}
		return number
	} else {
		if contact.BusinessName != "" {
			return contact.BusinessName + " (" + number + ")"
		} else if contact.FullName != "" {
			return contact.FullName + " (" + number + ")"
		} else {
			return evt.Info.Sender.User
		}
	}
}

func (c *WhatsAppClient) getAttachment(evt *events.Message) (bool, string, string, string, error) {
	var msg whatsmeow.DownloadableMessage
	var originalFileName string
	var txt string
	if vm := evt.Message.GetVideoMessage(); vm != nil {
		ext, err := c.getExtensionFromMimeType(vm.GetMimetype())
		if err != nil {
			return false, "", "", "", err
		}
		originalFileName = "video" + ext
		txt = vm.GetCaption()
		msg = evt.Message.GetVideoMessage()
	} else if am := evt.Message.GetAudioMessage(); am != nil {
		ext, err := c.getExtensionFromMimeType(am.GetMimetype())
		if err != nil {
			return false, "", "", "", err
		}
		originalFileName = "audio" + ext
		msg = evt.Message.GetAudioMessage()
	} else if dm := evt.Message.GetDocumentMessage(); dm != nil {
		ext, err := c.getExtensionFromMimeType(dm.GetMimetype())
		if err != nil {
			return false, "", "", "", err
		}
		originalFileName = evt.Message.GetDocumentMessage().GetFileName()
		if originalFileName == "" {
			originalFileName = "document" + ext
		}
		msg = evt.Message.GetDocumentMessage()
	} else if im := evt.Message.GetImageMessage(); im != nil {
		ext, err := c.getExtensionFromMimeType(im.GetMimetype())
		if err != nil {
			return false, "", "", "", err
		}
		originalFileName = "image" + ext
		msg = evt.Message.GetImageMessage()
		txt = im.GetCaption()
	}
	if evt.Message.GetConversation() == "" && msg == nil && evt.Message.GetExtendedTextMessage() == nil {
		c.SendText(*evt, "Dieser Nachrichtentyp wird leider nicht unterstützt :(")
		return false, "", "", "", fmt.Errorf("type unsupported")
	}

	if msg != nil {
		file, err := c.Client.Download(msg)
		if err != nil {
			return false, "", "", "", err
		}
		fName, err := saveBytesToTempFile(file)
		if err != nil {
			return false, "", "", "", err
		}
		return true, fName, originalFileName, txt, err
	} else {
		return false, "", "", "", nil
	}
}

func saveBytesToTempFile(data []byte) (string, error) {
	tmpfile, err := ioutil.TempFile("./tmp", "msg-media")
	if err != nil {
		return "", err
	}
	defer tmpfile.Close()

	if _, err := tmpfile.Write(data); err != nil {
		return "", err
	}
	return tmpfile.Name(), nil
}

func (c *WhatsAppClient) SendText(evt events.Message, err string) {
	c.Client.SendMessage(evt.Info.Chat, "", &waProto.Message{Conversation: proto.String(err)})
}

func (c *WhatsAppClient) SendTextWithJID(chatJID string, msg string) error {
	msgData := &waProto.Message{Conversation: proto.String(msg)}
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	_, err = c.Client.SendMessage(jid.ToNonAD(), "", msgData)
	return err
}

func (c *WhatsAppClient) SendInfoMessage(msg string) {
	if c.infoChat != "" {
		c.SendTextWithJID(c.infoChat, msg)
	}
}

func (c *WhatsAppClient) getExtensionFromMimeType(mimeType string) (string, error) {
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil {
		return "", err
	}
	if len(exts) > 0 {
		return exts[len(exts)-1], nil
	} else {
		return "", fmt.Errorf("no extension found")
	}
}
