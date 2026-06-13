package main

type PaymentMethod interface {
	Pay(usd int) int
	Cancel(id int)
}


type PaymentModule struct {
	Balance int
	PaymentInfo AllPayments 
}

func (p PaymentModule) Pay(tr CryptoBankPayPaller) {

}
func (p PaymentModule) Cancel() {}
func (p PaymentModule) Info() {}
func (p PaymentModule) AllInfo() {}



func main() {

}
