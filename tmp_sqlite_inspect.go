package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 3 {
		panic("usage: inspect <database> <sql>")
	}
	db, err := sql.Open("sqlite", "file:"+os.Args[1]+"?mode=ro")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	rows, err := db.Query(os.Args[2])
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		panic(err)
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			panic(err)
		}
		for index, value := range values {
			if index > 0 {
				fmt.Print("\t")
			}
			fmt.Printf("%s=%s", columns[index], value)
		}
		fmt.Println()
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
}
