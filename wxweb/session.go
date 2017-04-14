/*
Copyright 2017 wechat-go Authors. All Rights Reserved.
MIT License

Copyright (c) 2017

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package wxweb

import (
	"fmt"
	"github.com/mdp/qrterminal"
	"github.com/songtianyi/rrframework/config"
	"github.com/songtianyi/rrframework/logs"
	"github.com/songtianyi/rrframework/storage"
	"github.com/songtianyi/wechat-go/wxweb"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	WEB_MODE = iota + 1
	TERMINAL_MODE
)

var (
	DefaultCommon = &wxweb.Common{
		AppId:     "wx782c26e4c19acffb",
		LoginUrl:  "https://login.weixin.qq.com",
		Lang:      "zh_CN",
		DeviceID:  "e" + wxweb.GetRandomStringFromNum(15),
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_3) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/48.0.2564.109 Safari/537.36",
		CgiUrl:    "https://wx.qq.com/cgi-bin/mmwebwx-bin",
		CgiDomain: "https://wx.qq.com",
		SyncSrvs: []string{
			"webpush.wx.qq.com",
			"webpush.weixin.qq.com",
			"webpush.wechat.com",
			"webpush1.wechat.com",
			"webpush2.wechat.com",
		},
		UploadUrl:   "https://file.wx.qq.com/cgi-bin/mmwebwx-bin/webwxuploadmedia?f=json",
		MediaCount:  0,
		RedirectUri: "https://wx.qq.com/cgi-bin/mmwebwx-bin/webwxnewloginpage",
	}
)

type Session struct {
	WxWebCommon     *wxweb.Common
	WxWebXcg        *wxweb.XmlConfig
	Cookies         []*http.Cookie
	SynKeyList      *wxweb.SyncKeyList
	Bot             *wxweb.User
	Cm              *ContactManager
	QrcodePath      string //qrcode path
	QrcodeUUID      string //uuid
	RefreshFlag     chan struct{}
	HandlerRegister *HandlerRegister
}

func CreateSession(common *wxweb.Common, qrmode int) (*WxWebSession, error) {
	if common == nil {
		common = DefaultCommon
	}

	wxWebXcg := &wxweb.XmlConfig{}

	// get qrcode
	uuid, err := wxweb.JsLogin(common)
	if err != nil {
		return nil, err
	}
	logs.Debug(uuid)
	session := &Session{
		WxWebCommon:     common,
		WxWebXcg:        wxWebXcg,
		QrcodeUUID:      uuid,
		RefreshFlag:     make(chan struct{}, 1),
		HandlerRegister: CreateHandlerRegister(),
	}
	if qrmode == TERMINAL_MODE {
		qrterminal.Generate("https://login.weixin.qq.com/l/"+uuid, qrterminal.L, os.Stdout)
	} else if qrmode == WEB_MODE {
		qrcb, err := wxweb.QrCode(common, uuid)
		if err != nil {
			return nil, err
		}
		ls := rrstorage.CreateLocalDiskStorage("../public/qrcode/")
		if err := ls.Save(qrcb, "qrcode.jpg"); err != nil {
			return nil, err
		}
		session.QrcodePath = "/public/qrcode/" + uuid + ".jpg"
	}
	return session, nil
}

func (s *Session) LoginAndServe() error {

	var (
		err         error
		redirectUri string
	)
loop1:
	for {
		select {
		case <-time.After(5 * time.Second):
			redirectUri, err = wxweb.Login(s.WxWebCommon, s.QrcodeUUID, "0")
			if err != nil {
				logs.Error(err)
			} else {
				break loop1
			}
		}
	}
	logs.Debug(redirectUri)

	if s.Cookies, err = wxweb.WebNewLoginPage(s.WxWebCommon, s.WxWebXcg, redirectUri); err != nil {
		return err
	}

	jb, err := wxweb.WebWxInit(s.WxWebCommon, s.WxWebXcg)
	if err != nil {
		return err
	}

	jc, err := rrconfig.LoadJsonConfigFromBytes(jb)
	if err != nil {
		return err
	}

	s.SynKeyList, err = wxweb.GetSyncKeyListFromJc(jc)
	if err != nil {
		return err
	}
	s.Bot, _ = wxweb.GetUserInfoFromJc(jc)
	logs.Debug(s.Bot)
	ret, err := wxweb.WebWxStatusNotify(s.WxWebCommon, s.WxWebXcg, s.Bot)
	if err != nil {
		return err
	}
	if ret != 0 {
		return fmt.Errorf("WebWxStatusNotify fail, %d", ret)
	}

	cb, err := wxweb.WebWxGetContact(s.WxWebCommon, s.WxWebXcg, s.Cookies)
	if err != nil {
		return err
	}
	s.Cm, err = CreateContactManagerFromBytes(cb)
	if err != nil {
		return err
	}

	s.serve()
	return nil
}

func (s *Session) serve() {
	msg := make(chan []byte, 1000)
	// syncheck
	go s.producer(msg)
	for {
		select {
		case _, ok := <-s.RefreshFlag:
			if !ok {
				break
			}
		case m := <-msg:
			go s.consumer(m)
		}
	}
}
func (s *Session) producer(msg chan []byte) {
	for {
		select {
		case _, ok := <-s.RefreshFlag:
			if !ok {
				break
			}
		default:
			for _, v := range s.WxWebCommon.SyncSrvs {
				ret, sel, err := wxweb.SyncCheck(s.WxWebCommon, s.WxWebXcg, s.Cookies, v, s.SynKeyList)
				logs.Debug(v, ret, sel)
				if err != nil {
					logs.Error(err)
					continue
				}
				if ret == 0 {
					// check success
					if sel == 2 {
						// new message
						err := wxweb.WebWxSync(s.WxWebCommon, s.WxWebXcg, s.Cookies, msg, s.SynKeyList)
						if err != nil {
							logs.Error(err)
						}
					}
					if sel == 6 {
						s.RefreshFlag <- struct{}{}
					}
					break
				}
			}
		}
	}

}

func (s *Session) consumer(msg []byte) {
	// analize message
	jc, _ := rrconfig.LoadJsonConfigFromBytes(msg)
	msgCount, _ := jc.GetInt("AddMsgCount")
	if msgCount < 1 {
		// no msg
		return
	}
	msgis, _ := jc.GetInterfaceSlice("AddMsgList")
	for _, v := range msgis {
		msgi := v.(map[string]interface{})
		msgType := int(msgi["MsgType"].(float64))
		err, handles := s.HandlerRegister.Get(msgType)
		if err != nil {
			logs.Error(err)
			continue
		}
		for _, v := range handles {
			v.Run(s, analize(msgi))
		}
	}
}

func (s *Session) analize(msg map[string]interface{}) *ReceivedMessage {
	rmsg := &ReceivedMessage{
		MsgId:        msg["MsgId"].(string),
		Content:      msg["Content"].(string),
		FromUserName: msg["FromUserName"].(string),
		ToUserName:   msg["ToUserName"].(string),
	}

	if strings.Contains(rmsg.FromUserName, "@@") {
		rmsg.IsGroup = true
		// group message
		ss := strings.Split(rmsg.Content, ":")
		if len(ss) > 1 {
			rmsg.At = ss[0]
			rmsg.Content = strings.TrimPrefix(ss[1], "<br/>")
		}
	}
	return rmsg

}

// send text msg type 1
func (s *Session) SendText(msg, from, to string) {
	ret, err := wxweb.WebWxSendTextMsg(s.WxWebCommon, s.WxWebXcg, s.Cookies, from, to, msg)
	if ret != 0 {
		logs.Error(ret, err)
		return
	}
}

// send img, upload then send
func (s *Session) SendImg(path, from, to string) {
	ss := strings.Split(path, "/")
	b, err := ioutil.ReadFile(path)
	if err != nil {
		logs.Error(err)
		return
	}
	mediaId, err := wxweb.WebWxUploadMedia(s.WxWebCommon, s.WxWebXcg, s.Cookies, ss[len(ss)-1], b)
	if err != nil {
		logs.Error(err)
		return
	}
	ret, err := wxweb.WebWxSendMsgImg(s.WxWebCommon, s.WxWebXcg, s.Cookies, from, to, mediaId)
	if err != nil || ret != 0 {
		logs.Error(ret, err)
		return
	}
}

// get img by MsgId
func (s *Session) GetImg(msgId string) ([]byte, error) {
	return wxweb.WebWxGetMsgImg(s.WxWebCommon, s.WxWebXcg, s.Cookies, msgId)
}

// send gif, upload then send
func (s *Session) SendEmotion(path, from, to string) {
	ss := strings.Split(path, "/")
	b, err := ioutil.ReadFile(path)
	if err != nil {
		logs.Error(err)
		return
	}
	mediaId, err := wxweb.WebWxUploadMedia(s.WxWebCommon, s.WxWebXcg, s.Cookies, ss[len(ss)-1], b)
	if err != nil {
		logs.Error(err)
		return
	}
	ret, err := wxweb.WebWxSendEmoticon(s.WxWebCommon, s.WxWebXcg, s.Cookies, from, to, mediaId)
	if err != nil || ret != 0 {
		logs.Error(ret, err)
	}
}

func (s *Session) Stop() {
	close(s.RefreshFlag)
}
