package envconfig

import "github.com/HasonoCell/Orbit-DevOps/internal/platform/database"

// loadDatabasePool 读取本进程的显式预算；提高 Worker 并发不会悄悄放大数据库连接数。
func loadDatabasePool(maxOpen int) (database.PoolConfig, error) {
	config := database.DefaultPool(maxOpen)
	var err error
	config.MaxOpenConns, err = nonNegativeInteger("ORBIT_DEVOPS_DB_MAX_OPEN_CONNS", config.MaxOpenConns)
	if err != nil {
		return config, err
	}
	config.MaxIdleConns, err = nonNegativeInteger("ORBIT_DEVOPS_DB_MAX_IDLE_CONNS", config.MaxIdleConns)
	if err != nil {
		return config, err
	}
	config.ConnMaxIdleTime, err = duration("ORBIT_DEVOPS_DB_CONN_MAX_IDLE_TIME", config.ConnMaxIdleTime)
	if err != nil {
		return config, err
	}
	config.ConnMaxLifetime, err = duration("ORBIT_DEVOPS_DB_CONN_MAX_LIFETIME", config.ConnMaxLifetime)
	if err != nil {
		return config, err
	}
	config.PingTimeout, err = duration("ORBIT_DEVOPS_DB_PING_TIMEOUT", config.PingTimeout)
	if err != nil {
		return config, err
	}
	return config, config.Validate()
}
