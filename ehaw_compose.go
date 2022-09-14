// Copyright 2016 Martin Hebnes Pedersen (LA5NTA). All rights reserved.
// Use of this source code is governed by the MIT-license that can be
// found in the LICENSE file.

// A portable Winlink client for amateur radio email.

/*
	ehaw_compose.go creates an application specific interface for the
	Emergency Health & Welfare Message Service (eHaW) created by Bob
	Segrest [KO2F].  Implementation of this interface is limited to this
	file, 5 lines of code to add a 'Process' eHaW messages in main.go,
	and a sample eHaW_configuration.json file to be manually edited and
	deployed in the users pat folder.

	There is no expectation that this feature will be merged into the
	main PAT distribution pool.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/la5nta/wl2k-go/fbb"
)

type ehawConfig struct {
	Mysql_user     string `json:"mysql_user"`
	Mysql_password string `json:"mysql_password"`
	Mysql_host     string `json:"mysql_host"`
	Mysql_database string `json:"mysql_database"`
}

type msgStruct struct {
	msgId                      int
	msgSubject, msgTo, msgBody string
}

type msgQueue struct {
	msgId        int
	msgWinlinkId string
}

var db *sql.DB
var err error
var ehawCfg ehawConfig

func getEhawCfg() error {
	// read the json file
	basePathLen := len(fOptions.ConfigPath) - len("config.json")
	cfgPath := fOptions.ConfigPath[:basePathLen] + "eHaW_config.json"
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	// parse the values into ehawCfg
	eCfg := make(map[string]ehawConfig)
	err = json.Unmarshal(data, &eCfg)
	if err != nil {
		return err
	}
	// and copy them into the eHaW Config structure
	ehawCfg = eCfg["eHaW"]
	return err
}

func getEhawEmail() ([]msgStruct, error) {
	// open the eHaW database
	conStr := ehawCfg.Mysql_user + ":" +
		ehawCfg.Mysql_password + "@" +
		ehawCfg.Mysql_host + "/" +
		ehawCfg.Mysql_database
	db, err = sql.Open("mysql", conStr)
	if err != nil {
		fmt.Println(err.Error())
	}
	defer db.Close()
	// read the formatted messages from the eHaw Database
	rows, err := db.Query("SELECT * FROM buildMsg ORDER BY msgId")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// structure the eHaW submitted messages for Processing in PAT
	var msgs []msgStruct
	for rows.Next() {
		var msg msgStruct
		if err := rows.Scan(&msg.msgId, &msg.msgSubject, &msg.msgTo, &msg.msgBody); err != nil {
			return msgs, err
		}
		msgs = append(msgs, msg)
	}
	if err = rows.Err(); err != nil {
		return msgs, err
	}
	return msgs, nil
}

func processEhawEmail(ctx context.Context, args []string) {
	var msgs []msgStruct
	// load the eHaW configuration information
	err := getEhawCfg()
	if err != nil {
		log.Println(err)
		fmt.Println("Failed to load eHaW config!")
	}
	// get submitted email msgs from eHaW
	msgs, err = getEhawEmail()
	if err != nil {
		log.Println(err)
		fmt.Println(" There was a problem getting messages from the eHaW database")
		return
	}
	if len(msgs) < 1 {
		fmt.Println("There are no submitted eHaW messages to process at this time.")
	} else {
		// process each eHaW submitted message
		for msg := 0; msg < len(msgs); msg++ {
			// display the message for Moderation by a PAT operator
			fmt.Println("\n\r\r")
			fmt.Println("eHaW Id: ", msgs[msg].msgId)
			fmt.Println("Subject: ", msgs[msg].msgSubject)
			fmt.Println("To:      ", msgs[msg].msgTo, "\n\r\r")
			fmt.Println(msgs[msg].msgBody)
			fmt.Println("\n\r")
			// the PAT operator decides what happens next
			fmt.Print("Accept, Decline, or Ignore: ")
			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				log.Println(err)
			}
			input = strings.TrimSpace(input)
			input = strings.ToUpper(input)
			switch input {
			case "D", "DECLINE":
				// if declined, status is updated in the eHaW database
				//  and the submitted message is effectively closed
				// add code here to mark the eHaW message record as Declined
				updateEhawMsg(msgs[msg].msgId, "Declined", "")
				fmt.Println("this message has been Declined and will not be sent.")
			case "I", "IGNORE":
				// if Ignored, no action is taken and the process move forward to
				//  the next message.  The ignored message will be included the
				//  next time eHaW messages are processed
				fmt.Println("This message will be ignored for now.")
			case "A", "ACCEPT":
				// if the message is accepted, we build a quick list of MIDs from
				//  the PAT out folder
				beforeList, err := getMsgBoxList("out")
				if err != nil {
					fmt.Println("Error reading PAT Out folder")
					return
				}
				// then create the in PAT almost the same way it is done in the
				//  PAT cli interface
				err = ehawComposeMessage(msgs[msg].msgSubject, msgs[msg].msgTo, msgs[msg].msgBody)
				if err != nil {
					log.Println(err)
				}
				// after the message is created, we immediately build a new list of MIDs
				//  in the PAT out folder
				afterList, err := getMsgBoxList("out")
				if err != nil {
					log.Println(err)
				}
				// by comparing the 2 out folder lists we can identify the Message ID (MID)
				// created for the new message
				newMsgId := getMsgIds(beforeList, afterList)
				// finally, we can change the message status to Accepted and store the MID
				//  in eHaW for reference in a future process cycle
				updateEhawMsg(msgs[msg].msgId, "Accepted", newMsgId[0])
			default:
				fmt.Println("Invalid action, ignoring this message for now.")
			}
		}
	}
	// after all the messages are processed, we read a list previously Accepted
	//  messages from eHaW, look for matching MIDs in the PAT sent folder, and
	//  for this that match set the message status in eHaW to sent
	newMsgsSent, err := updateSentEhawMsgStatus()
	if err != nil {
		fmt.Println("Problem updating eHaW sent message status")
	}

	// if sent messages were updated, tell the PAT operator
	if newMsgsSent > 0 {
		fmt.Println(strconv.Itoa(newMsgsSent) + " eHaW messages set to Sent")
	}

}

func updateEhawMsg(Id int, status string, newMsgId string) {
	// open the eHaW database
	conStr := ehawCfg.Mysql_user + ":" +
		ehawCfg.Mysql_password + "@" +
		ehawCfg.Mysql_host + "/" +
		ehawCfg.Mysql_database
	db, err = sql.Open("mysql", conStr)
	if err != nil {
		fmt.Println(err.Error())
	}
	defer db.Close()
	// yes its ugly SQL, but this is the way MySQL updates a record
	stmt, err := db.Prepare("UPDATE msgQueue SET msgStatus=?, msgWinlinkId=? WHERE msgId =?")
	if err != nil {
		log.Println(err)
		return
	}
	res, err := stmt.Exec(status, newMsgId, strconv.Itoa(Id))
	if err != nil {
		log.Println(err)
		return
	}
	log.Println(res, "eHaW record updated")
}

func updateSentEhawMsgStatus() (int, error) {
	newEhawMsgsSentCount := 0
	// open the eHaW database
	conStr := ehawCfg.Mysql_user + ":" +
		ehawCfg.Mysql_password + "@" +
		ehawCfg.Mysql_host + "/" +
		ehawCfg.Mysql_database
	db, err = sql.Open("mysql", conStr)
	if err != nil {
		fmt.Println(err.Error())
	}
	defer db.Close()
	// get a list of eHaW messages that were previously accepted
	rows, err := db.Query("SELECT msgId, msgWinlinkId FROM msgQueue WHERE msgStatus='Accepted' AND msgWinlinkId IS NOT NULL")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	// now count the Accepted messages and create a slice of the messages returned
	var accepted []msgQueue
	rowCount := 0
	for rows.Next() {
		// get the record
		var msg msgQueue
		if err := rows.Scan(&msg.msgId, &msg.msgWinlinkId); err != nil {
			return 0, err
		}
		// save the values later
		accepted = append(accepted, msg)
		// and count the IDs to look for
		rowCount += 1
	}
	// if there are IDs to look for
	if rowCount > 0 {
		// get a list of PAT messages that have been sent
		var sentMsgs []string
		sentMsgs, err = getMsgBoxList("sent")

		// for each previously accepted eHaW message
		for ehaw := 0; ehaw < len(accepted); ehaw++ {
			// see if the accepted WinlinkId is in the sent list
			for sent := 0; sent < len(sentMsgs); sent++ {
				// if we find a match
				if accepted[ehaw].msgWinlinkId == sentMsgs[sent] {
					// prepare an update statement (its a Go thing...)
					stmt, err := db.Prepare("UPDATE msgQueue SET msgStatus=? WHERE msgId=?")
					if err != nil {
						log.Println(err)
						return newEhawMsgsSentCount, err
					}
					// and use the eHaW msgId to change Approved to Sent
					res, err := stmt.Exec("Sent", strconv.Itoa(accepted[ehaw].msgId))
					if err != nil {
						log.Println(err, res)
						return newEhawMsgsSentCount, err
					}
					newEhawMsgsSentCount++
				}
			}
		}
	}
	// when done, return the count
	return newEhawMsgsSentCount, err
}

func getMsgBoxList(msgBox string) ([]string, error) {
	var oMsgs []string
	// open the folder (out, sent) and get a list of message files
	msgBoxPath := fOptions.MailboxPath + "\\" + fOptions.MyCall + "\\" + msgBox
	msgBoxDirectory, err := os.Open(msgBoxPath)
	if err != nil {
		fmt.Println("out folder not found")
		return oMsgs, err
	}
	defer msgBoxDirectory.Close()
	// now strip off the .b2f extension and return a list of MIDs
	msgBoxFiles, err := msgBoxDirectory.Readdirnames(0)
	if err != nil {
		return oMsgs, err
	}
	for _, name := range msgBoxFiles {
		if (len(name) > 4) && (name[len(name)-4:] == ".b2f") {
			tName := bytes.Trim([]byte(name), ".b2f")
			oMsgs = append(oMsgs, string(tName))
		}
	}
	return oMsgs, err
}

func getMsgIds(before, after []string) []string {
	// this function returns a list of new message IDs
	// from after that did not exist before
	// hopefully, there will only be 1
	mb := make(map[string]struct{}, len(before))
	for _, x := range before {
		mb[x] = struct{}{}
	}
	var newMsgId []string
	for _, x := range after {
		if _, found := mb[x]; !found {
			newMsgId = append(newMsgId, x)
		}
	}
	return newMsgId
}

func ehawComposeMessage(subject string, recipients string, body string) error {
	// all we are really doing here is using the standard PAT cli feature
	//  if there was a way to open it programmatically I wouldn't have modified PAT
	//  Perhaps, we can write an API for PAT as a future project
	msg := fbb.NewMessage(fbb.Private, fOptions.MyCall)
	msg.SetFrom(fOptions.MyCall)
	r := strings.Split(recipients, ";")
	for _, to := range r {
		msg.AddTo(to)
	}
	msg.SetSubject(subject)
	msg.SetBody(string(body))
	postMessage(msg)
	return err
}
