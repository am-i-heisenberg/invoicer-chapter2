// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor: Julien Vehent jvehent@mozilla.com [:ulfr]
package main

//go:generate ./version.sh

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

type invoicer struct {
	db *gorm.DB
}

func main() {
	var (
		iv  invoicer
		err error
	)
	var db *gorm.DB
	if os.Getenv("INVOICER_USE_POSTGRES") != "" {
		db, err = gorm.Open("postgres", fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
			os.Getenv("INVOICER_POSTGRES_USER"),
			os.Getenv("INVOICER_POSTGRES_PASSWORD"),
			os.Getenv("INVOICER_POSTGRES_HOST"),
			os.Getenv("INVOICER_POSTGRES_DB"),
			os.Getenv("INVOICER_POSTGRES_SSLMODE"),
		))
	} else {
		db, err = gorm.Open("sqlite3", "invoicer.db")
	}
	if err != nil {
		panic("failed to connect database")
	}
	iv.db = db
	iv.db.AutoMigrate(&Invoice{}, &Charge{})
	iv.db.LogMode(true)

	//initialize CSRF Token
	CSRFKey = make([]byte, 128)
	_, err = rand.Read(CSRFKey)
	if err != nil {
		log.Fatal("error initializing CSRF Key:", err)
	}

	// register routes
	r := mux.NewRouter()
	r.HandleFunc("/", getIndex).Methods("GET")
	r.HandleFunc("/__heartbeat__", getHeartbeat).Methods("GET")
	r.HandleFunc("/invoice/{id:[0-9]+}", iv.getInvoice).Methods("GET")
	r.HandleFunc("/invoice", iv.postInvoice).Methods("POST")
	r.HandleFunc("/invoice/{id:[0-9]+}", iv.putInvoice).Methods("PUT")
	r.HandleFunc("/invoice/{id:[0-9]+}", iv.deleteInvoice).Methods("DELETE")
	r.HandleFunc("/invoice/delete/{id:[0-9]+}", iv.deleteInvoice).Methods("GET")
	r.HandleFunc("/__version__", getVersion).Methods("GET")

	// handle static files
	r.Handle("/statics/{staticfile}",
		http.StripPrefix("/statics/", http.FileServer(http.Dir("./statics"))),
	).Methods("GET")

	// all set, start the http handler
	log.Fatal(http.ListenAndServe(":8080", r))
}

type Invoice struct {
	gorm.Model
	IsPaid      bool      `json:"is_paid"`
	Amount      int       `json:"amount"`
	PaymentDate time.Time `json:"payment_date"`
	DueDate     time.Time `json:"due_date"`
	Charges     []Charge  `json:"charges"`
}

type Charge struct {
	gorm.Model
	InvoiceID   int     `gorm:"index"  json:"invoice_id"`
	Type        string  `json:"type"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
}

func (iv *invoicer) getInvoice(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	log.Println("getting invoice id", vars["id"])
	var i1 Invoice
	id, _ := strconv.Atoi(vars["id"])
	iv.db.First(&i1, id)
	fmt.Printf("%+v\n", i1)
	if i1.ID == 0 {
		httpError(w, http.StatusNotFound, "No invoice id %s", vars["id"])
		return
	}
	iv.db.Where("invoice_id = ?", i1.ID).Find(&i1.Charges)
	jsonInvoice, err := json.Marshal(i1)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to retrieve invoice id %d: %s", vars["id"], err)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonInvoice)
}

func (iv *invoicer) postInvoice(w http.ResponseWriter, r *http.Request) {
	log.Println("posting new invoice")
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body: %s", err)
		return
	}
	var i1 Invoice
	err = json.Unmarshal(body, &i1)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to parse request body: %s", err)
		return
	}
	// make sure the IDs are null before inserting
	i1.ID = 0
	for i := 0; i < len(i1.Charges); i++ {
		i1.Charges[i].ID = 0
		i1.Charges[i].InvoiceID = 0
	}
	iv.db.Create(&i1)
	iv.db.Last(&i1)
	log.Printf("%+v\n", i1)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(fmt.Sprintf("created invoice %d", i1.ID)))
}

func (iv *invoicer) putInvoice(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	log.Println("updating invoice", vars["id"])
	var i1 Invoice
	iv.db.First(&i1, vars["id"])
	if i1.ID == 0 {
		httpError(w, http.StatusNotFound, "No invoice id %s", vars["id"])
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body: %s", err)
		return
	}
	err = json.Unmarshal(body, &i1)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to parse request body: %s", err)
		return
	}
	iv.db.Save(&i1)
	iv.db.First(&i1, vars["id"])
	log.Printf("%+v\n", i1)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(fmt.Sprintf("updated invoice %d", i1.ID)))
}

func (iv *invoicer) deleteInvoice(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	if !checkCSRFToken(r.Header.Get("X-CSRF-Token")) {
		w.WriteHeader(http.StatusNotAcceptable)
		w.Write([]byte("Missing CSRF Token"))
		return
	}
	log.Println("deleting invoice", vars["id"])
	var i1 Invoice
	id, _ := strconv.Atoi(vars["id"])
	iv.db.Where("invoice_id = ?", id).Delete(Charge{})
	i1.ID = uint(id)
	iv.db.Delete(&i1)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(fmt.Sprintf("deleted invoice %d", i1.ID)))
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Security-Policy", "default-src 'self';")
	w.Write([]byte(`
<!DOCTYPE html>
<html>
    <head>
        <title>Invoicer Web</title>
        <script src="statics/jquery-1.12.4.min.js"></script>
        <script src="statics/invoicer-cli.js"></script>
        <link href="statics/style.css" rel="stylesheet">
    </head>
    <body>
	<h1>Invoicer Web</h1>
        <p class="desc-invoice"></p>
        <div class="invoice-details">
        </div>
        <h3>Request an invoice by ID</h3>
        <form id="invoiceGetter" method="GET">
            <label>ID :</label>
            <input id="invoiceid" type="text" />
            <input type="hidden" name="CSRFToken" value="` + makeCSRFToken() + `">
            <input type="submit" />
        </form>
        <form id="invoiceDeleter" method="DELETE">
            <label>Delete this invoice</label>
            <input type="submit" />
        </form>
    </body>
</html>`))
}

func getHeartbeat(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("I am alive"))
}

// handleVersion returns the current version of the API
func getVersion(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(fmt.Sprintf(`{
"source": "https://github.com/Securing-DevOps/invoicer",
"version": "%s",
"commit": "%s",
"build": "https://circleci.com/gh/Securing-DevOps/invoicer/"
}`, version, commit)))
}

func httpError(w http.ResponseWriter, errorCode int, errorMessage string, args ...interface{}) {
	log.Printf("%d: %s", errorCode, fmt.Sprintf(errorMessage, args...))
	http.Error(w, fmt.Sprintf(errorMessage, args...), errorCode)
	return
}

var CSRFKey []byte

func makeCSRFToken() string {
	msg := make([]byte, 32)
	rand.Read(msg)
	mac := hmac.New(sha256.New, CSRFKey)
	mac.Write(msg)
	return base64.StdEncoding.EncodeToString(msg) + `;` + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func checkCSRFToken(token string) bool {
	mac := hmac.New(sha256.New, CSRFKey)
	tokenParts := strings.Split(token, ";")
	if len(tokenParts) != 2 {
		return false
	}
	msg := tokenParts[0]
	messageMAC, _ := base64.StdEncoding.DecodeString(tokenParts[1])
	mac.Write([]byte(msg))
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC)
}
