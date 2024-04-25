# mo_dump
MatrixOne supports logical backups through the mo_dump utility. mo_dump is a command-line utility designed to generate logical backups of the MatrixOne database. It produces SQL statements that can be used to recreate database objects and data.

## 使用方法

### 语法结构

```
./mo-dump -u ${user} -p ${password} -h ${host} -P ${port} -db ${database} [--local-infile=true] [--enable-escape=false] [-csv] [-no-data] [-tbl ${table}...] -net-buffer-length ${net-buffer-length} > {dumpfilename.sql}
```

**参数释义**

- **-u [user]**：连接 MatrixOne 服务器的用户名。只有具有数据库和表读取权限的用户才能使用 `mo-dump` 实用程序，默认值 dump。

- **-p [password]**：MatrixOne 用户的有效密码。默认值：111。

- **-h [host]**：MatrixOne 服务器的主机 IP 地址。默认值：127.0.0.1

- **-P [port]**：MatrixOne 服务器的端口。默认值：6001

- **-db [数据库名称]**：必需参数。要备份的数据库的名称。可以指定多个数据库，数据库名称之间用 `,` 分隔。

- **-net-buffer-length [数据包大小]**：数据包大小，即 SQL 语句字符的总大小。数据包是 SQL 导出数据的基本单位，如果不设置参数，则默认 1048576 Byte（1M），最大可设置 16777216 Byte（16M）。假如这里的参数设置为 16777216 Byte（16M），那么，当要导出大于 16M 的数据时，会把数据拆分成多个 16M 的数据包，除最后一个数据包之外，其它数据包大小都为 16M。

- **-csv**：默认值为 false。当设置为 true 时表示导出的数据为 *CSV* 格式。

- **--local-infile**：默认值为 true，仅在参数 **-csv** 设置为 true 时生效。表示支持本地导出 *CSV* 文件。

- **-tbl [表名]**：可选参数。如果参数为空，则导出整个数据库。如果要备份指定表，则可以在命令中指定多个 `-tbl` 和表名。

- **-no-data**：默认值为 false。当设置为 true 时表示不导出数据，仅导出表结构。

- **-enable-escape**：默认值为 false，仅在参数 **-csv** 设置为 true 时生效。表示对特殊字符开启转义，可以避免一些兼容性问题。


### 构建 mo-dump 二进制文件
__Tips:__ 由于 `mo-dump` 是基于 Go 语言进行开发，所以你同时需要安装部署 <a href="https://go.dev/doc/install" target="_blank">Go</a> 语言。

1. 执行下面的代码即可从 MatrixOrigin/mo_dump 源代码构建 `mo-dump` 二进制文件：

    ```
    git clone https://github.com/matrixorigin/mo_dump.git
    cd mo_dump
    make build
    ```

2. 你可以在 mo_dump 文件夹中找到 `mo-dump` 可执行文件：*mo-dump*。

!!! note
    构建好的 `mo-dump` 文件也可以在相同的硬件平台上工作。但是需要注意在 x86 平台中构建的 `mo-dump` 二进制文件在 Darwin ARM 平台中则无法正常工作。你可以在同一套操作系统和硬件平台内构建并使用 `mo-dump` 二进制文件。`mo-dump` 目前只支持 Linux 和 macOS。

## 如何使用 `mo-dump` 导出 MatrixOne 数据库

`mo-dump` 在命令行中非常易用。参见以下步骤示例，导出 *sql* 文件格式完整数据库：

在你本地计算机上打开终端窗口，输入以下命令，连接到 MatrixOne，并且导出数据库：

```
./mo-dump -u username -p password -h host_ip_address -P port -db database > exporteddb.sql
```

例如，如果你在与 MatrixOne 实例相同的服务器中启动终端，并且你想要生成单个数据库的备份，请运行以下命令。该命令将在 *t.sql* 文件中生成 **t** 数据库的结构和数据的备份。*t.sql* 文件将与您的 `mo-dump` 可执行文件位于同一目录中。

```
./mo-dump -u root -p 111 -h 127.0.0.1 -P 6001 -db t > t.sql
```

如果你想将数据库 *t* 内的表导出为 *CSV* 格式，参考使用下面的命令：

```
./mo-dump -u root -p 111  -db t -csv --local-infile=false > ttt.csv
```

如果要在数据库中生成单个表的备份，可以运行以下命令。该命令将生成命名为 *t* 的数据库的 *t1* 表的备份，其中包含 *t.sql* 文件中的结构和数据。

```
./mo-dump -u root -p 111 -db t -tbl t1 > t1.sql
```

* `mo-dump`  支持导出多个数据库。[在 -db后接数据库名]
例如
```mysql
mo-dump -u dump -p xxx -h 127.0.0.1 -P 6001 -db db1,db2,db3 > /tm/db1-db2-db3.sql
mo-dump -u dump -p xxx -h 127.0.0.1 -P 6001 -db all > /tm/all-dbs.sql
```

* `mo-dump`  不仅支持导出多个数据库，还支持导出多个数据库表。
例如
```mysql
mo-dump -u dump -p xxx -h 127.0.0.1 -P 6001 -db db1 -tbl tbl1,tbl2,tbl3 > /tm/all-dbs.sql
```

## 限制
* `mo-dump` 暂不支持只导出数据库的结构或数据。如果你想在没有数据库结构的情况下生成数据的备份，或者仅想导出数据库结构，那么，你需要手动拆分 `.sql` 文件。
