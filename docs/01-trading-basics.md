# How trading works

The first goal is to understand one complete trade well enough to check the software's decisions yourself. We will use a fictional stock, ABC, and a simulated account.

## 1. What are you buying?

A stock is a small ownership stake in a company. You can earn money if its price rises or it pays dividends, and lose money if its value falls. An exchange-traded fund (ETF) holds a portfolio of investments and trades in shares too. A ticker is an instrument's short identifier. [SEC: stocks](https://www.investor.gov/introduction-investing/investing-basics/investment-products/stocks), [SEC: ETFs](https://www.investor.gov/introduction-investing/investing-basics/investment-products/mutual-funds-and-exchange-traded-2).

When you trade an existing stock, you usually buy from or sell to another market participant. A broker accepts your order and routes it for execution. An exchange or another trading venue brings orders together. Your program talks to the broker's API; the LLM's written recommendation is several steps away from owning shares. [SEC: executing an order](https://www.investor.gov/introduction-investing/investing-basics/how-stock-markets-work/executing-order).

**Investing** often means holding assets for years. **Trading** usually means changing positions more frequently to benefit from price movements. For this project, daily decisions can lead to holdings lasting several days or longer; a daily run does not require a daily trade.

## 2. Where does the price come from?

Buyers offer bids; sellers offer asks. The best bid is the highest current buying price, and the best ask is the lowest current selling price. Their difference is the spread. The last completed trade is a different piece of information. [SEC: bid and ask](https://www.investor.gov/introduction-investing/investing-basics/glossary/ask-price).

For example, ABC might show a $99.95 bid, a $100.05 ask, and a $100 last trade. An immediate small purchase might cost $100.05. Immediately selling it back might receive $99.95, losing $0.10 per share even if the quoted market stays unchanged. Available quantities matter too.

Prices change as people revise the prices and quantities they will trade. A good company can have a falling stock price if its results disappoint expectations. Reading positive news does not automatically create a profitable trade: the price may already reflect it.

A daily price **bar** summarizes the session's opening price, highest price, lowest price, closing price, and trading volume, often abbreviated **OHLCV**. A candlestick is a way to draw those prices. These summaries do not reveal the complete sequence of trades within the day.

**Liquidity** describes how readily you can trade without a large price concession. **Slippage** is the difference between an expected price and the actual fill. A fill is an executed part of an order. A large order can fill in several pieces. [SEC: executing an order](https://www.investor.gov/introduction-investing/investing-basics/how-stock-markets-work/executing-order).

## 3. What instructions can you send?

| Order | Meaning | Tradeoff |
| --- | --- | --- |
| Market | Execute at available market prices | You do not fix the price |
| Limit | Buy at this price or lower; sell at this price or higher | It may never fill |
| Stop | Trigger a market order when a specified price is reached | The eventual fill can be worse than the trigger |

An order being accepted does not mean it filled. Track its quantity, filled quantity, price, and final status. A stop at $95 cannot promise a $95 exit if the market jumps below it. [SEC: order types](https://www.investor.gov/introduction-investing/investing-basics/how-stock-markets-work/types-orders).

We will initially simulate purchases and sales at the next session's opening price with an explicit cost model. That is a modeling assumption, not a promise of real execution.

## 4. Follow the money through one trade

Assume $10,000 initial cash, no dividends, a $1 fee on each order, and the following completed fills. These numbers are invented for teaching.

| Event | Cash | Shares | Share price used to value the account | Total account value |
| --- | ---: | ---: | ---: | ---: |
| Start | $10,000 | 0 | $100 | $10,000 |
| Buy 10 shares at $100; pay $1 | $8,999 | 10 | $100 | $9,999 |
| Later market price is $105 | $8,999 | 10 | $105 | $10,049 |
| Sell all 10 at $105; pay $1 | $10,048 | 0 | $105 | $10,048 |

The **position** was the 10 shares you held. The **portfolio** is all positions plus cash. **Account equity** is cash plus the current market value of the positions, assuming no borrowing.

```text
Gross trade profit = 10 × ($105 − $100) = $50
Profit after trading fees = $50 − $2 = $48
Account return = $48 / $10,000 = 0.48%
```

The share price rose 5%, but the account gained 0.48% because only about one tenth of the account was invested. Before selling, the gain is **unrealized**; after selling, it is **realized**. Both affect your account's value while positions are marked to current prices.

If the project also spent $3 on LLM calls, its economic profit after those costs would be $45. Keep that expense visible even if it was paid outside the trading account. Fees reduce returns; free commissions do not remove spreads, slippage, or software costs. [SEC: fees and expenses](https://www.investor.gov/introduction-investing/general-resources/news-alerts/alerts-bulletins/investor-bulletins/updated).

The same shares at $90 would create a $100 gross loss. The LLM's confidence would not change the arithmetic.

## 5. What is a strategy?

A strategy is a repeatable rule for what to trade, when to enter, how much to hold, and when to exit. "Buy promising companies" leaves most of those questions unanswered.

Here are candidate hypotheses to test, with no claim that they work:

| Approach | Plain meaning | Example failure |
| --- | --- | --- |
| Trend following | Buy because a sustained rise may continue | Repeated reversals cause losses |
| Mean reversion | Buy because an unusually large fall may reverse | The business keeps deteriorating |
| Fundamentals | Compare the business's earnings, debt, growth, and price | A good estimate takes too long to matter |
| News or events | Estimate how new information changes expectations | The market reacted before your order |

An **edge** means a repeatable advantage after costs. It must be measured. For example, winning $30 on 40% of trades and losing $10 on 60% yields an average $6 gross profit per trade: `0.4 × 30 − 0.6 × 10`. Accuracy alone misses the size of wins and losses.

A useful LLM hypothesis is: "Given the same price data, adding time-stamped news analysis improves results after trading and inference costs." That is an experiment we can evaluate.

## 6. How do you control risk?

**Position sizing** determines how much money one decision exposes. **Volatility** describes fluctuations; **drawdown** measures a decline from a previous account peak. A fall from $10,000 to $8,000 is a 20% drawdown and requires a 25% gain to recover.

Different holdings can still move together. Diversification reduces dependence on a single investment but cannot eliminate market losses. [SEC: diversification](https://www.investor.gov/introduction-investing/getting-started/asset-allocation).

Buying shares creates a **long** position. Selling shares you already own reduces it; selling your last shares makes you **flat**. **Short selling** involves selling stock you do not own, generally borrowing shares, with the aim of buying them back for less. [SEC: short sales](https://www.investor.gov/introduction-investing/investing-basics/glossary/short-sales).

**Leverage** means taking exposure larger than the capital funding it, often through borrowing or derivatives. Our first simulator uses long positions funded by cash, whole shares, and a configured maximum position size. This makes its accounting easier to understand and verify.

## 7. How do you find out whether it works?

**Paper trading** runs the program as new data arrives, with simulated orders and money. It tests the real decision loop, scheduling, API failures and bookkeeping without risking cash. Simulated fills still omit parts of real execution; for example, Alpaca documents differences involving market impact, latency slippage, queue position and dividends. [Alpaca: paper trading](https://docs.alpaca.markets/us/docs/paper-trading).

This project is a paper-trading agent. It decides only on new data as it arrives, and its orders fill at the next session's open, a price nobody knew when the decision was made. A model also cannot "remember" how a period it is trading turned out, because that period has not happened yet.

Measure account return after costs, worst drawdown, how much capital was exposed, trading frequency, and results versus a benchmark. A **benchmark** is a comparison strategy, such as holding the same eligible stocks without active decisions. **Sharpe ratio** compares excess return with its variability; a large estimate from a short sample is weak evidence.

A few good weeks prove little. Watch results accumulate, change one setting at a time, and keep the buy-and-hold comparison in view: if the agent cannot beat simply holding the same stocks after costs, it is not adding value.

## 8. Check your understanding

Before moving on, explain these in your own words:

1. Why can a correct prediction still produce a losing trade?
2. What is the difference between a recommendation, an order, and a fill?
3. If 10% of your account is invested and that stock falls 20%, approximately how much does the account lose, ignoring costs? **Answer: 2%.**
4. Why is a confident explanation insufficient evidence of an edge?
5. Why must a simulator track cash and positions across days?

Next: [architecture](02-architecture.md).
