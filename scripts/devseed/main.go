// devseed 向目标 GaussDB/openGauss 实例灌入集成测试所需的结构与数据。
// 仅供开发测试使用：
//
//	go run ./scripts/devseed "host=127.0.0.1 port=15433 user=gaussdb password=Gaussdb@123 dbname=postgres sslmode=disable"
package main

import (
	"context"
	"fmt"
	"os"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法: devseed <dsn>")
		os.Exit(1)
	}
	ctx := context.Background()
	conn, err := gaussdbgo.Connect(ctx, os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接失败:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	// 清理旧的测试对象（尽力而为：对象或模式不存在时忽略错误）。
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS sales CASCADE`,
		`DROP VIEW IF EXISTS public.active_users`,
		`DROP TABLE IF EXISTS public.events CASCADE`,
		`DROP TABLE IF EXISTS public.events_2026`,
		`DROP TABLE IF EXISTS public.empty_t`,
		`DROP TABLE IF EXISTS sales.orders`,
		`DROP TABLE IF EXISTS public.users CASCADE`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			fmt.Fprintf(os.Stderr, "提示（可忽略）: %q: %v\n", sql, err)
		}
	}

	stmts := []string{
		`CREATE SCHEMA sales`,
		`CREATE TABLE public.users (id serial PRIMARY KEY, name text NOT NULL, email text UNIQUE, created_at timestamptz DEFAULT now())`,
		`COMMENT ON TABLE public.users IS '用户表'`,
		`COMMENT ON COLUMN public.users.email IS '邮箱'`,
		`CREATE INDEX idx_users_name ON public.users(name)`,
		`CREATE TABLE sales.orders (id serial PRIMARY KEY, user_id int REFERENCES public.users(id), amount numeric(10,2), status text)`,
		`CREATE INDEX idx_orders_status ON sales.orders(status)`,
		`INSERT INTO public.users (name, email) VALUES ('alice','a@x.com'),('bob','b@x.com')`,
		`INSERT INTO sales.orders (user_id, amount, status) VALUES (1, 99.90, 'paid'), (2, 49.50, 'pending'), (1, 19.99, 'paid')`,
		`CREATE VIEW public.active_users AS SELECT id, name, email FROM public.users WHERE id > 0`,
		`CREATE TABLE public.empty_t (id int)`,
	}
	for _, sql := range stmts {
		if _, err := conn.Exec(ctx, sql); err != nil {
			fmt.Fprintf(os.Stderr, "执行失败 %q: %v\n", sql, err)
			os.Exit(1)
		}
	}

	// 分区表：openGauss 与 PostgreSQL 语法不同，依次尝试。
	partitionDDLs := [][]string{
		{ // openGauss / GaussDB
			`CREATE TABLE public.events (id int, ts timestamp) PARTITION BY RANGE (ts) (PARTITION p1 VALUES LESS THAN ('2027-01-01'), PARTITION p2 VALUES LESS THAN (MAXVALUE))`,
		},
		{ // PostgreSQL
			`CREATE TABLE public.events (id int, ts timestamp) PARTITION BY RANGE (ts)`,
			`CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		},
	}
	var seeded bool
	for _, ddls := range partitionDDLs {
		ok := true
		for _, sql := range ddls {
			if _, err := conn.Exec(ctx, sql); err != nil {
				ok = false
				break
			}
		}
		if ok {
			seeded = true
			break
		}
	}
	if !seeded {
		fmt.Fprintln(os.Stderr, "分区表创建失败（两种语法均不支持）")
		os.Exit(1)
	}
	fmt.Println("测试数据就绪：public.users / sales.orders / active_users / events(分区表)")
}
