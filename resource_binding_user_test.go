package main_test

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/cloudfoundry/terraform-provider-csbpg/csbpg"
)

const (
	adminUsername         = "postgres"
	cloudsqlsuperuser     = "cloudsqlsuperuser"
	cloudsqlsuperpassword = "password"
	hostname              = "localhost"
	defaultDatabase       = "default"
)

//go:embed "testfixtures/ssl_postgres/certs/ca.crt"
var postgresSSLCACert string

//go:embed "testfixtures/ssl_postgres/certs/client.crt"
var postgresSSLClientCert string

//go:embed "testfixtures/ssl_postgres/keys/client.key"
var postgresSSLClientKey string

var _ = Describe("SSL Postgres Bindings", func() {
	var session *gexec.Session
	var adminUserURI, adminPassword, database string
	var port int

	BeforeEach(func() {
		var err error
		adminPassword = uuid.New().String()
		database = uuid.New().String()
		port = freePort()

		cmd := exec.Command(
			"docker", "run",
			"-e", fmt.Sprintf("POSTGRES_PASSWORD=%s", adminPassword),
			"-e", fmt.Sprintf("POSTGRES_DB=%s", defaultDatabase),
			"-p", fmt.Sprintf("%d:5432", port),
			"--mount", "source=ssl_postgres,destination=/mnt",
			"-t", "postgres",
			"-c", "config_file=/mnt/pgconf/postgresql.conf",
			"-c", "hba_file=/mnt/pgconf/pg_hba.conf",
		)
		session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			db, err := sql.Open("postgres", buildConnectionString(adminUsername, adminPassword, port, defaultDatabase))
			if err != nil {
				return err
			}
			defer func(db *sql.DB) {
				_ = db.Close()
			}(db)
			return db.Ping()
		}).WithTimeout(10 * time.Second).WithPolling(time.Second).Should(Succeed())

		replicateGCPPostgresEnv(port, database, adminPassword)

		adminUserURI = buildConnectionString(adminUsername, adminPassword, port, database)
	})

	AfterEach(func() {
		session.Terminate()
	})

	It("creates a binding user", func() {
		dataOwnerRole := "dataOwnerRole_" + uuid.New().String()
		bindingUsername := "bindingUsername_" + uuid.New().String()
		bindingPassword := uuid.New().String()
		applyHCL(fmt.Sprintf(`
		provider "csbpg" {
		  host            = "%s"
		  port            = %d
		  username        = "%s"
		  password        = "%s"
		  database        = "%s"
		  data_owner_role = "%s"
		
		  sslrootcert = <<EOF
%s
EOF
		  clientcert {
    		cert = <<EOF
%s
EOF
    		key  = <<EOF
%s
EOF
  	      }
		}

		resource "csbpg_binding_user" "binding_user" {
		  username = "%s"
		  password = "%s"
		}
		`, hostname, port, cloudsqlsuperuser, cloudsqlsuperpassword, database, dataOwnerRole,
			postgresSSLCACert, postgresSSLClientCert, postgresSSLClientKey,
			bindingUsername, bindingPassword),
			func(state *terraform.State) error {
				By("CHECKING RESOURCE CREATE")

				db, err := sql.Open("postgres", adminUserURI)
				Expect(err).NotTo(HaveOccurred())

				By("checking that the data owner role is created")
				rows, err := db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has not been created", dataOwnerRole))

				By("checking that the binding user is created")
				rows, err = db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", bindingUsername))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has not been created", bindingUsername))

				By("checking that the binding user is a member of the data owner role")
				rows, err = db.Query(fmt.Sprintf("SELECT pg_has_role('%s', '%s', 'member')", bindingUsername, dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				var result bool
				Expect(rows.Next()).To(BeTrue(), "pg_has_role() query failed")
				Expect(rows.Scan(&result)).To(Succeed())
				Expect(result).To(BeTrue(), "binding user is not a member of the data_owner_role")

				By("by adding data as the new user")
				bindingDB, err := sql.Open("postgres", buildConnectionString(bindingUsername, bindingPassword, port, database))
				Expect(err).NotTo(HaveOccurred())

				_, err = bindingDB.Exec("CREATE SCHEMA foo")
				Expect(err).NotTo(HaveOccurred())

				_, err = bindingDB.Exec("CREATE TABLE foo.bar(PK   INT primary key,  Name VARCHAR(30))")
				Expect(err).NotTo(HaveOccurred())

				_, err = bindingDB.Exec("INSERT INTO foo.bar (pk, name) VALUES(1,'Test name');")
				Expect(err).NotTo(HaveOccurred())

				return nil
			},
			func(state *terraform.State) error {
				By("CHECKING RESOURCE DELETE")
				db, err := sql.Open("postgres", adminUserURI)
				Expect(err).NotTo(HaveOccurred())

				By("checking that the data owner role is not deleted")
				rows, err := db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has been deleted", dataOwnerRole))

				By("checking that the binding user is deleted")
				rows, err = db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", bindingUsername))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeFalse(), fmt.Sprintf("role %q still exists", bindingUsername))

				By("checking that data persists")
				Expect(query(db, "SELECT name FROM foo.bar where pk = 1")).To(ConsistOf("Test name"))

				return nil
			})
	})

	It("can create multiple binding user", func() {
		dataOwnerRole := uuid.New().String()
		bindingUsername1 := uuid.New().String()
		bindingPassword1 := uuid.New().String()

		bindingUsername2 := uuid.New().String()
		bindingPassword2 := uuid.New().String()
		applyHCL(fmt.Sprintf(`
		provider "csbpg" {
		  host            = "%s"
		  port            = %d
		  username        = "%s"
		  password        = "%s"
		  database        = "%s"
		  data_owner_role = "%s"

		  sslrootcert = <<EOF
%s
EOF
		  clientcert {
    		cert = <<EOF
%s
EOF
    		key  = <<EOF
%s
EOF
		  }
		}

		resource "csbpg_binding_user" "binding_user_1" {
		  username = "%s"
		  password = "%s"
		}

		resource "csbpg_binding_user" "binding_user_2" {
		  username = "%s"
		  password = "%s"
		}
		`, hostname, port, adminUsername, adminPassword, database, dataOwnerRole,
			postgresSSLCACert, postgresSSLClientCert, postgresSSLClientKey,
			bindingUsername1, bindingPassword1, bindingUsername2, bindingPassword2),
			func(state *terraform.State) error {
				By("CHECKING RESOURCE CREATE")

				db, err := sql.Open("postgres", adminUserURI)
				Expect(err).NotTo(HaveOccurred())

				By("checking that the data owner role is created")
				rows, err := db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has not been created", dataOwnerRole))

				By("checking that the binding user is created")
				rows, err = db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", bindingUsername1))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has not been created", bindingUsername1))

				By("checking that the binding user is a member of the data owner role")
				rows, err = db.Query(fmt.Sprintf("SELECT pg_has_role('%s', '%s', 'member')", bindingUsername1, dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				var result bool
				Expect(rows.Next()).To(BeTrue(), "pg_has_role() query failed")
				Expect(rows.Scan(&result)).To(Succeed())
				Expect(result).To(BeTrue(), "binding user is not a member of the data_owner_role")

				Expect(query(db, fmt.Sprintf("SELECT pg_has_role('%s', '%s', 'member')", bindingUsername1, dataOwnerRole))).To(ConsistOf(true))
				return nil
			},
			func(state *terraform.State) error {
				By("CHECKING RESOURCE DELETE")

				db, err := sql.Open("postgres", adminUserURI)
				Expect(err).NotTo(HaveOccurred())

				By("checking that the data owner role is not deleted")
				rows, err := db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", dataOwnerRole))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeTrue(), fmt.Sprintf("role %q has been deleted", dataOwnerRole))

				By("checking that both binding users are deleted")
				rows, err = db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", bindingUsername1))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeFalse(), fmt.Sprintf("role %q still exists", bindingUsername1))

				rows, err = db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = '%s'", bindingUsername2))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows.Next()).To(BeFalse(), fmt.Sprintf("role %q still exists", bindingUsername2))

				return nil
			})
	})

	It("will re-attach an existing legacy user", func() {
		dataOwnerRole := "dataOwnerRole_" + uuid.New().String()
		bindingUsername := "bindingUsername_" + uuid.New().String()
		bindingPassword := uuid.New().String()
		userConnectionString := buildConnectionString(bindingUsername, bindingPassword, port, database)

		By("CREATING PRE-EXISTING USER AS PER THE LEGACY BROKER")
		db, err := sql.Open("postgres", adminUserURI)
		defer func(db *sql.DB) {
			_ = db.Close()
		}(db)
		Expect(err).NotTo(HaveOccurred())

		adminStatements := []string{
			fmt.Sprintf("CREATE ROLE binding_group with role %s", pq.QuoteIdentifier(cloudsqlsuperuser)),
			fmt.Sprintf("CREATE USER %s WITH PASSWORD %s IN ROLE binding_group", pq.QuoteIdentifier(bindingUsername), pq.QuoteLiteral(bindingPassword)),
			fmt.Sprintf("GRANT ALL ON DATABASE %s TO %s", pq.QuoteIdentifier(database), pq.QuoteIdentifier(bindingUsername)),
			fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(bindingUsername), pq.QuoteIdentifier(cloudsqlsuperuser)),
		}

		for _, adminStatement := range adminStatements {
			_, err = db.Exec(adminStatement)
			Expect(err).NotTo(HaveOccurred())
		}

		By("SETTING UP USER DATA STRUCTURES")
		userDb, err := sql.Open("postgres", userConnectionString)
		Expect(err).NotTo(HaveOccurred())

		userStatements := []string{
			fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT ALL ON TABLES TO binding_group", pq.QuoteIdentifier(bindingUsername)),
			fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT ALL ON SEQUENCES TO binding_group", pq.QuoteIdentifier(bindingUsername)),
			fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT ALL ON FUNCTIONS TO binding_group", pq.QuoteIdentifier(bindingUsername)),
			"CREATE TABLE T1 (PK INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR(30))",
			"CREATE FUNCTION F1() RETURNS VARCHAR\nAS $$ SELECT 'f1' $$\nLANGUAGE SQL",
		}
		for _, userStatement := range userStatements {
			_, err = userDb.Exec(userStatement)
			Expect(err).NotTo(HaveOccurred())
		}

		By("ADDING AND READING DATA")
		insertResult, err := userDb.Exec("INSERT INTO T1 (PK, NAME) VALUES (1, 'Example row')")
		Expect(err).NotTo(HaveOccurred())
		Expect(insertResult.RowsAffected()).To(BeEquivalentTo(1))

		Expect(query(userDb, "SELECT F1() || PK || NAME FROM T1")).To(ConsistOf("f11Example row"))
		err = userDb.Close()
		Expect(err).NotTo(HaveOccurred())

		applyHCL(fmt.Sprintf(`
		provider "csbpg" {
		  host            = "%s"
		  port            = %d
		  username        = "%s"
		  password        = "%s"
		  database        = "%s"
		  data_owner_role = "%s"
		
		  sslrootcert = <<EOF
%s
EOF
		  clientcert {
    		cert = <<EOF
%s
EOF
    		key  = <<EOF
%s
EOF
  	      }
		}

		resource "csbpg_binding_user" "binding_user" {
		  username = "%s"
		  password = "%s"
		}
		`, hostname, port, cloudsqlsuperuser, cloudsqlsuperpassword, database, dataOwnerRole,
			postgresSSLCACert, postgresSSLClientCert, postgresSSLClientKey,
			bindingUsername, bindingPassword), func(state *terraform.State) error {
			By("ESTABLISHING BINDING USER CONNECTION")
			bindingDb, err := sql.Open("postgres", userConnectionString)
			Expect(err).NotTo(HaveOccurred())
			By("READING THE PREVIOUSLY EXISTING DATA")
			Expect(query(bindingDb, "select f1() || NAME from t1")).To(ConsistOf("f1Example row"))
			Expect(err).NotTo(HaveOccurred())
			return bindingDb.Close()
		}, func(state *terraform.State) error {
			By("CHECKING RESOURCE DELETE")
			db, err := sql.Open("postgres", adminUserURI)
			Expect(err).NotTo(HaveOccurred())

			By("checking that the binding user is deleted")
			rows, err := db.Query(fmt.Sprintf("SELECT FROM pg_catalog.pg_roles WHERE rolname = %s", pq.QuoteLiteral(bindingUsername)))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows.Next()).To(BeFalse(), fmt.Sprintf("role %q still exists", bindingUsername))
			return nil
		})
	})
})

func replicateGCPPostgresEnv(port int, database, adminPassword string) {
	adminConn, err := sql.Open("postgres", buildConnectionString(adminUsername, adminPassword, port, defaultDatabase))
	Expect(err).NotTo(HaveOccurred())
	defer func(adminConn *sql.DB) {
		_ = adminConn.Close()
	}(adminConn)

	_, err = adminConn.Exec(fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD '%s' NOSUPERUSER CREATEDB CREATEROLE", cloudsqlsuperuser, cloudsqlsuperpassword))
	Expect(err).NotTo(HaveOccurred())

	cloudSQLSuperUserConn, err := sql.Open("postgres", buildConnectionString(cloudsqlsuperuser, cloudsqlsuperpassword, port, defaultDatabase))
	Expect(err).NotTo(HaveOccurred())
	defer func(cloudSQLSuperUserConn *sql.DB) {
		_ = cloudSQLSuperUserConn.Close()
	}(cloudSQLSuperUserConn)

	_, err = cloudSQLSuperUserConn.Exec(fmt.Sprintf("CREATE DATABASE \"%s\"", database))
	Expect(err).NotTo(HaveOccurred())
}

func buildConnectionString(username, password string, port int, database string) string {
	return strings.Join([]string{
		"host=" + hostname,
		fmt.Sprintf("port=%d", port),
		"user=" + username,
		"password=" + password,
		"database=" + database,
		"sslmode=verify-ca",
		"sslinline=true",
		fmt.Sprintf("sslcert='%s'", postgresSSLClientCert),
		fmt.Sprintf("sslkey='%s'", postgresSSLClientKey),
		fmt.Sprintf("sslrootcert='%s'", postgresSSLCACert),
	}, " ")
}

func query(db *sql.DB, query string) (any, error) {
	var result []any
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		var row any
		err = rows.Scan(&row)
		if err != nil {
			return nil, err
		}

		result = append(result, row)
	}
	return result, nil
}

func applyHCL(hcl string, checkOnCreate, checkOnDestroy resource.TestCheckFunc) {
	resource.Test(GinkgoT(), resource.TestCase{
		IsUnitTest: true, // means we don't need to set TF_ACC
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"csbpg": func() (*schema.Provider, error) { return csbpg.Provider(), nil },
		},
		CheckDestroy: checkOnDestroy,
		Steps: []resource.TestStep{{
			ResourceName: "csbpg_shared_role.shared_role",
			Config:       hcl,
			Check:        checkOnCreate,
		}},
	})
}
