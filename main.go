package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	gdax "github.com/mynonce/gdax"
)

type atState int

const (
	waiting atState = iota
	entering
	entered
	exiting
	exit
	exited
)

func (a atState) String() string {
	names := map[atState]string{
		waiting:  "Waiting",
		entering: "Entering",
		entered:  "Entered",
		exiting:  "Exiting",
		exit:     "Exit",
		exited:   "Exited",
	}
	return names[a]
}

var (
	awsProfile = flag.String("profile", "jdub", "AWS Access Key Profile")
	stream     = flag.String("stream", "gdax_order_price_avg", "your stream name")
	region     = flag.String("region", "us-east-1", "your AWS region")

	currentState       *state
	updateBookPriceAvg bool
	lastPrice          float64
	signalStrength     int
	trendingBuy        bool
	lastBookPriceAvg   BookPriceAvg
)

// BookPriceAvg ...
type BookPriceAvg struct {
	ProductID string  `json:"PRODUCT_ID"`
	Price     float64 `json:"TICKER_SYMBOL_AVG"`
}

func doEvery(d time.Duration, f func(time.Time)) {
	for x := range time.Tick(d) {
		f(x)
	}
}

func logInfo(t time.Time) {
	fmt.Printf("Book Price Avg: %f, Last Trade Price: %f, - %s ( %d )\n", lastBookPriceAvg.Price, lastPrice, currentState.AT, signalStrength)
}

func fetchSize(productID string) *float64 {
	acc, err := gdax.GDAXClient.AccountClient.FindAccountByCurrency(productID)
	if err != nil {
		log.Fatal("fetchSize:", err)
		return nil
	}
	return &acc.Available
}

func fetchAsk() *float64 {
	book, err := gdax.GDAXClient.BookClient.GetBookByProduct("BTC-USD")
	if err != nil {
		log.Println("fetchLastTradePrice:", err)
		return nil
	}
	if len(book.Asks) > 0 {
		return &book.Asks[0].Price
	}
	return nil
}

func fetchBid() *float64 {
	book, err := gdax.GDAXClient.BookClient.GetBookByProduct("BTC-USD")
	if err != nil {
		log.Println("fetchLastTradePrice:", err)
		return nil
	}
	if len(book.Bids) > 0 {
		return &book.Bids[0].Price
	}
	return nil
}

func fetchLastTradePrice(t time.Time) {
	book, err := gdax.GDAXClient.BookClient.GetBookByProduct("BTC-USD")
	if err != nil {
		log.Println("fetchLastTradePrice:", err)
		return
	}
	if (len(book.Asks) > 0) && (len(book.Bids) > 0) {
		if book.Asks[0].Size > book.Bids[0].Size {
			lastPrice = book.Asks[0].Price
		} else {
			lastPrice = book.Bids[0].Price
		}
	}
}

func toggleUpdatingBookPriceAvg(t time.Time) {
	updateBookPriceAvg = true
}

func logOrders(t time.Time) {
	if currentState != nil {
		for o := range currentState.settledOrders {
			log.Printf("Settled - %v", o)
		}
	}
}

func updateTradeSignal(t time.Time) {
	switch currentState.AT {
	case waiting:
		if lastBookPriceAvg.Price < lastPrice {
			signalStrength++
		} else {
			signalStrength = 0
		}
		if signalStrength > 20 {
			signalStrength = 0
			currentState.buySignal <- true // inform state machine to enter position
		}
	case entering:
		if lastBookPriceAvg.Price > lastPrice {
			signalStrength++
		} else {
			signalStrength = 0
		}
		if signalStrength > 20 {
			signalStrength = 0
			gdax.GDAXClient.OrderClient.CancelOrder(currentState.openedOrder.ID)
		}
	case entered:
		if lastBookPriceAvg.Price > lastPrice {
			signalStrength++
		} else {
			signalStrength = 0
		}
		if signalStrength > 20 {
			signalStrength = 0
			currentState.AT = exit // inform state machine to exit position
		}
	default:
		// Short circuit the state machine if the price drops below our loss
		l := len(currentState.settledOrders)
		if (currentState.holding) && (l > 0) {
			p := currentState.settledOrders[l-1].Price
			loss := p - (p * 0.01)
			if lastPrice < loss {
				currentState.AT = exit
			}
		}
	}
}

type state struct {
	buySignal      chan bool
	sellSignal     chan bool
	AT             atState
	holding        bool
	openedOrder    *gdax.Order
	settledOrders  []gdax.Order
	quickSellOrder *gdax.Order
}

type transition func() transition

func runStateMachine() {
	/*
		1.  No Open Orders, Cash in account, Buy Signal - Open Buy Limit Order (WAITING -> ENTERING)
		2.  Open Orders									- Check Order Status (ENTERING -> ENTERED || -> EXITING)
		3.  Settled Buy Orders							- Open Sell Limit Order (.005 or .5%) (ENTERED -> EXITED || -> EXIT)
		4.  Sell Signal									- Cancel Sell Limit Order, Open Sell Order at ask (EXIT -> EXITED)
		5.  Exit Open Orders							- Cancel Open Orders, hurry (EXITING)
		6.  Settled Sell Orders							- Reset State (EXITED)
	*/
	startTransition := waitingEvent
	for transition := startTransition; transition != nil; {
		transition = transition()
	}
}

func waitingEvent() transition {
	log.Println("WAITING...")
	currentState = &state{
		AT:        waiting,
		buySignal: make(chan bool),
	}

	<-currentState.buySignal

	return enteringEvent
}

func enteringEvent() transition {
	log.Println("ENTERING...")

	if currentState.AT == waiting {
		currentState.AT = entering
		amt := fetchSize("USD")
		price := fetchBid()
		bid := float64(int(*price*1e2)) / 1e2
		size := float64(int(*amt / *price * 1e8)) / 1e8
		o := gdax.NewLimitBuyOrder(
			"BTC-USD",
			bid,
			size,
		)

		log.Println("New Order - ", *amt, *price, bid, size)
		newOrder, err := gdax.GDAXClient.OrderClient.CreateOrder(*o)
		if err != nil {
			log.Println("ENTERING: failed to create order:", err)
			return enteringEvent
		}

		currentState.openedOrder = &newOrder
	}

	for range time.Tick(time.Second * 2) {
		// This check allows the main thread to direct us to exit position
		if currentState.AT == exit {
			return exitEvent
		}

		o, err := gdax.GDAXClient.OrderClient.GetOrdersByID(currentState.openedOrder.ID)
		if err != nil {
			log.Println("ENTERING: failed to get order:", err)
			// assumption is we canceled the order manually, goto waiting...
			return waitingEvent
		}
		if o.Settled {
			currentState.holding = true
			currentState.openedOrder = &o
			currentState.settledOrders = append(currentState.settledOrders, o)
			return enteredEvent
		}
	}
	return enteredEvent
}

func enteredEvent() transition {
	log.Println("ENTERED...")
	if currentState.AT == entering {
		currentState.AT = entered
		price := currentState.openedOrder.Price + (currentState.openedOrder.Price * 0.005)
		price = float64(int(price*1e2)) / 1e2

		size := fetchSize("BTC")
		currentState.quickSellOrder = gdax.NewLimitSellOrder(
			"BTC-USD",
			price,
			*size,
		)
		newOrder, err := gdax.GDAXClient.OrderClient.CreateOrder(*currentState.quickSellOrder)
		if err != nil {
			log.Println("ENTERED: failed to create order:", err)
			currentState.AT = entering
			return enteredEvent
		}

		currentState.openedOrder = &newOrder
	}

	for range time.Tick(time.Second * 2) {
		// This check allows the main thread to direct us to exit position
		if currentState.AT == exit {
			return exitEvent
		}
		o, err := gdax.GDAXClient.OrderClient.GetOrdersByID(currentState.openedOrder.ID)
		if err != nil {
			log.Println("ENTERED: failed to get order:", err)
			return waitingEvent
		}
		if o.Settled {
			currentState.holding = false
			currentState.openedOrder = &o
			currentState.settledOrders = append(currentState.settledOrders, o)
			return exitedEvent
		}
	}
	return exitedEvent
}

func exitedEvent() transition {
	// clean up
	log.Println("Exited...")
	currentState.AT = exited
	_, err := gdax.GDAXClient.OrderClient.CancelAllOrders()
	if err != nil {
		log.Println("EXITED: failed to cancel all open orders")
		return exitedEvent
	}
	return waitingEvent
}

func exitEvent() transition {
	// exit now
	log.Println("Exit...")

	currentState.AT = exit
	if currentState.holding {
		_, err := gdax.GDAXClient.OrderClient.CancelAllOrders()
		if err != nil {
			log.Println("EXIT: failed to cancel all open orders")
			return exitEvent
		}

		price := fetchAsk()
		amt := float64(int(*price*1e2)) / 1e2

		size := fetchSize("BTC")
		o := gdax.NewLimitSellOrder(
			"BTC-USD",
			amt,
			*size,
		)
		o.TimeInForce = "GTT"
		o.CancelAfter = "min"

		newOrder, err := gdax.GDAXClient.OrderClient.CreateOrder(*o)
		if err != nil {
			log.Println("EXIT: failed to create order:", err)
			return exitEvent
		}

		currentState.openedOrder = &newOrder

		for range time.Tick(time.Second * 2) {
			o, err := gdax.GDAXClient.OrderClient.GetOrdersByID(currentState.openedOrder.ID)
			if err != nil {
				log.Println("EXIT: failed to get order:", err)
				return exitEvent
			}
			if &o == nil {
				return exitEvent
			}
			if o.Settled {
				currentState.holding = false
				currentState.openedOrder = &o
				currentState.settledOrders = append(currentState.settledOrders, o)
				return exitedEvent
			}
		}
	}

	return waitingEvent
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	s := session.Must(session.NewSessionWithOptions(session.Options{
		Config:  aws.Config{Region: aws.String(*region)},
		Profile: *awsProfile,
	}))

	// consumer
	c, err := NewConsumer(
		*stream,
		s,
		WithCheckpoint(FileCheckPoint{}),
	)
	if err != nil {
		log.Fatalf("consumer error: %v", err)
	}

	go runStateMachine()

	fetchLastTradePrice(time.Now())

	go doEvery(time.Second*5, fetchLastTradePrice)
	go doEvery(time.Second, updateTradeSignal)
	go doEvery(time.Second, toggleUpdatingBookPriceAvg)

	go doEvery(time.Second*2, logInfo)
	go doEvery(time.Minute, logOrders)

	// start
	err = c.Scan(context.TODO(), func(r *kinesis.Record) bool {
		if !updateBookPriceAvg {
			return true
		}
		updateBookPriceAvg = false
		bookPriceAvg := BookPriceAvg{}
		err := json.Unmarshal(r.Data, &bookPriceAvg)
		if err != nil {
			log.Fatal(err)
			return false
		}
		lastBookPriceAvg = BookPriceAvg{
			Price:     bookPriceAvg.Price,
			ProductID: bookPriceAvg.ProductID,
		}
		return true // continue scanning
	})
	if err != nil {
		log.Fatalf("scan error: %v", err)
	}
}
