package main

import (
	"bufio"
	"fmt"
	"go-repo-downloader/models"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jinzhu/gorm"
	"github.com/joshdk/go-junit"
)

func Measure(db *gorm.DB, repoDir string, repository models.Repository, commitID uint, currCommit *object.Commit) {
	measurement := &models.Measurement{RepositoryID: repository.ID}
	models.CreateMeasurement(db, measurement)

	dt := time.Now()
	fmt.Println(currCommit.Hash.String() + " - " + dt.String())

	err := Checkout(repository.Name, currCommit.Hash.String())
	if err != nil {
		fmt.Println("Error checkout commit " + currCommit.Hash.String() + " " + err.Error())
		log.Println("Error checkout commit " + currCommit.Hash.String() + " " + err.Error())
	} else {

		switch buildtool := checkBuildTool(repoDir); buildtool {
		case "":
			fmt.Println("ATTENTION: Maven or Gradle files not found in ", repoDir)
		case "maven":
			MvnInstall(repoDir)
			ok := MvnCompile(repoDir)
			if ok {
				MeasureMavenTests(db, repoDir, commitID, *measurement)
				JacocoTestCoverage(db, repoDir, "maven", "maven", measurement.ID)
				mavenClasspath := GetMavenDependenciesClasspath(repoDir)
				for _, file := range listJavaFiles(repoDir) {
					MeasureRandoopTests(db, repoDir, file, "maven", mavenClasspath, commitID, *measurement)
				}
				JacocoTestCoverage(db, repoDir, "randoop", "maven", measurement.ID)
			}
		case "gradle":
			projectPaths := getProjectPaths(repoDir)
			for _, projectPath := range projectPaths {
				buildPath := repoDir + string(os.PathSeparator) + projectPath
				ok := GradleBuild(buildPath)
				if ok {
					MeasureGradleTests(db, buildPath, commitID, *measurement)
					JacocoTestCoverage(db, buildPath, "gradle", "gradle", measurement.ID)
					gradleClasspath := GetGradleDependenciesClasspath(buildPath)
					for _, file := range listJavaFiles(buildPath) {
						MeasureRandoopTests(db, buildPath, file, "gradle", gradleClasspath, commitID, *measurement)
					}
					JacocoTestCoverage(db, buildPath, "randoop", "gradle", measurement.ID)
				}
			}
		}

	}
}

func MeasureMavenTests(db *gorm.DB, repoDir string, commitID uint, measurement models.Measurement) {
	testResults, ok := MvnTest(db, repoDir, measurement.ID)
	if ok {
		for ind := range testResults {
			mr := &models.Test{MeasurementID: measurement.ID,
				Type:        "maven",
				ClassName:   testResults[ind].ClassName,
				CommitID:    commitID,
				TestsRun:    testResults[ind].TestsRun,
				Failures:    testResults[ind].Failures,
				Errors:      testResults[ind].Errors,
				Skipped:     testResults[ind].Skipped,
				TimeElapsed: testResults[ind].TimeElapsed}
			models.CreateTest(db, mr)
		}
	} else {
		log.Println("********************** CRITICAL ERROR ***************")
		log.Println("successAfter is false measuring maven tests")
	}
}

func MeasureGradleTests(db *gorm.DB, repoDir string, commitID uint, measurement models.Measurement) {
	testResults, ok := GradleTest(db, repoDir, measurement.ID)
	if ok {
		for ind := range testResults {
			mr := &models.Test{MeasurementID: measurement.ID,
				Type:        "gradle",
				ClassName:   testResults[ind].ClassName,
				CommitID:    commitID,
				TestsRun:    testResults[ind].TestsRun,
				Failures:    testResults[ind].Failures,
				Errors:      testResults[ind].Errors,
				Skipped:     testResults[ind].Skipped,
				TimeElapsed: testResults[ind].TimeElapsed}
			models.CreateTest(db, mr)
		}
		fmt.Printf("repoDir gradle tests: %s", repoDir)
		suites, err := junit.IngestDir(repoDir + "/build/test-results/test/")
		if err != nil {
			log.Fatalf("failed to ingest JUnit xml %v", err)
		}
		for _, suite := range suites {
			fmt.Println(suite.Name)
			for _, test := range suite.Tests {
				fmt.Printf("  %s\n", test.Name)
				if test.Error != nil {
					fmt.Printf("    %s: %s\n", test.Status, test.Error.Error())
				} else {
					fmt.Printf("    %s %f\n", test.Status, test.Duration.Seconds())
				}
			}
		}

	} else {
		log.Println("********************** CRITICAL ERROR ***************")
		log.Println("successAfter is false measuring maven tests")
	}
}

func MeasureRandoopTests(db *gorm.DB, repoDir, file, buildTool, buildToolClasspath string, commitID uint, measurement models.Measurement) {
	//java -classpath ${RANDOOP_JAR} randoop.main.Main gentests --classlist=myclasses.txt --time-limit=60
	//Randoop prints out is the name of the JUnit files containing the tests it generated

	dir, pack := parseProjectPath(file)
	if dir != "" {
		dir += string(os.PathSeparator)
	}

	path := strings.Split(pack, ".java")[0]
	// fmt.Println("path: ", path)
	randoopJar := "${RANDOOP_JAR}"
	cpSep := ":"
	if runtime.GOOS == "windows" {
		randoopJar = "%RANDOOP_JAR%"
		cpSep = ";"
	}

	envRandoopJar := os.Getenv("RANDOOP_JAR")
	// remove old tests
	// deleteOldRandoopTests()

	classpath := ""
	switch buildTool {
	case "maven":
		classpath += dir + "target" + string(os.PathSeparator) + "classes" + cpSep
	case "gradle":
		classpath += dir + "build" + string(os.PathSeparator) + "classes" + cpSep +
			dir + "build" + string(os.PathSeparator) + "classes" + string(os.PathSeparator) + "java" + string(os.PathSeparator) + "main" + cpSep +
			dir + "build" + string(os.PathSeparator) + "classes" + string(os.PathSeparator) + "java" + string(os.PathSeparator) + "test"
	}
	classpath += buildToolClasspath
	className := strings.ReplaceAll(path, string(os.PathSeparator), ".")

	fmt.Println("------------------------------------------------ Generating Randoop tests for " + file + "...")
	okGen := generateRandoopTests(classpath, cpSep, randoopJar, envRandoopJar, className)

	// Compile and run the tests. (The classpath should include the code under test, the generated tests, and JUnit files junit.jar and hamcrest-core.jar. Classes in java.util.* are always on the Java classpath, so the myclasspath part is not needed in this particular example, but it is shown because you will usually need to supply it.)
	// export JUNITPATH=.../junit.jar:.../hamcrest-core.jar
	// javac -classpath .:$JUNITPATH ErrorTest*.java RegressionTest*.java -sourcepath .:path/to/files/under/test/
	// java -classpath .:$JUNITPATH:myclasspath org.junit.runner.JUnitCore ErrorTest
	// java -classpath .:$JUNITPATH:myclasspath org.junit.runner.JUnitCore RegressionTest

	if okGen {
		okComp := compileRandoopTests(classpath, cpSep)
		if okComp {
			testTime, numTests, perfMetrics, okTest := runRandoopTests(classpath, cpSep)
			if okTest {
				r := &models.Test{MeasurementID: measurement.ID,
					Type:      "randoop",
					ClassName: file,
					CommitID:  commitID,
					TestsRun:  numTests,
					// Failures:    failures,
					// Errors:      errors,
					// Skipped:     skipped,
					TimeElapsed: testTime}
				testID, err := models.CreateTest(db, r)
				if err != nil {
					log.Println("Error creating randoop: " + err.Error())
					fmt.Println("Error creating randoop: " + err.Error())
				} else {
					for _, perfMetric := range perfMetrics {
						rr := &models.TestResources{
							TestID: testID,
							Type:   "randoop",
							Resources: models.Resources{
								CpuPercent:        perfMetric.CpuPercent,
								MemPercent:        perfMetric.MemoryPercent,
								MemoryInfoStat:    *perfMetric.MemoryInfo,
								IOCountersStat:    *perfMetric.IOCounters,
								PageFaultsStat:    *perfMetric.PageFaults,
								AvgStat:           *perfMetric.Load,
								VirtualMemoryStat: *perfMetric.VirtualMemory,
								// SwapMemoryStat:    *perfMetric.SwapMemory,
								// CPUTime:           perfMetric.CPUTime,
								// DiskIOCounters:    perfMetric.DiskIOCounters,
								// NetIOCounters:     perfMetric.NetIOCounters,
							},
						}
						models.CreateTestResources(db, rr)
						for _, cpuTime := range perfMetric.CPUTimes {
							models.CreateCPUTimes(db, &models.CPUTimes{
								MeasurementResourcesID: rr.ID,
								CPU:                    cpuTime.CPU,
								User:                   cpuTime.User,
								System:                 cpuTime.System,
								Idle:                   cpuTime.Idle,
								Nice:                   cpuTime.Nice,
								Iowait:                 cpuTime.Iowait,
								Irq:                    cpuTime.Irq,
								Softirq:                cpuTime.Softirq,
								Steal:                  cpuTime.Steal,
								Guest:                  cpuTime.Guest,
								GuestNice:              cpuTime.GuestNice,
							})

						}

						for i, diskIOCounter := range perfMetric.DiskIOCounters {
							models.CreateDiskIOCounters(db, &models.DiskIOCounters{
								MeasurementResourcesID: rr.ID,
								Device:                 i,
								ReadCount:              diskIOCounter.ReadCount,
								MergedReadCount:        diskIOCounter.MergedReadCount,
								WriteCount:             diskIOCounter.WriteCount,
								MergedWriteCount:       diskIOCounter.MergedWriteCount,
								ReadBytes:              diskIOCounter.ReadBytes,
								WriteBytes:             diskIOCounter.WriteBytes,
								ReadTime:               diskIOCounter.ReadTime,
								WriteTime:              diskIOCounter.WriteTime,
								IopsInProgress:         diskIOCounter.IopsInProgress,
								IoTime:                 diskIOCounter.IoTime,
								WeightedIO:             diskIOCounter.WeightedIO,
								Name:                   diskIOCounter.Name,
								SerialNumber:           diskIOCounter.SerialNumber,
								Label:                  diskIOCounter.Label,
							})
						}

						for i, netIOCounter := range perfMetric.NetIOCounters {
							models.CreateNetIOCounters(db, &models.NetIOCounters{
								MeasurementResourcesID: rr.ID,
								NICID:                  uint(i),
								Name:                   netIOCounter.Name,
								BytesSent:              netIOCounter.BytesSent,
								BytesRecv:              netIOCounter.BytesRecv,
								PacketsSent:            netIOCounter.PacketsSent,
								PacketsRecv:            netIOCounter.PacketsRecv,
								Errin:                  netIOCounter.Errin,
								Errout:                 netIOCounter.Errout,
								Dropin:                 netIOCounter.Dropin,
								Dropout:                netIOCounter.Dropout,
								Fifoin:                 netIOCounter.Fifoin,
								Fifoout:                netIOCounter.Fifoout,
							})
						}
					}
				}
			}

		}

	}

	// CollectRandoopMetrics(repoDir, repository.Name, commit.PreviousCommitHash, change.From.Name, commit.CommitHash, change.To.Name, changeObj.ID)
}

// func MeasureChanges(db *gorm.DB, repoDir string, repository models.Repository, commit models.Commit, changes object.Changes) {
// 	//randoop
// 	for _, change := range changes {
// 		// fmt.Println(change.From.Name)
// 		// fmt.Println(change.To.Name)
// 		// fmt.Println(change.Action())
// 		// fmt.Println(change.Files())
// 		// fmt.Println("------------------- start")
// 		// fmt.Println(change.Patch())

// 		patch, _ := change.Patch()
// 		diff, _ := diffparser.Parse(patch.String())

// 		//files
// 		count := 0
// 		for _, file := range diff.Files {
// 			// fmt.Println("************************** file: ", file)

// 			sc := fmt.Sprintf("%d", count)

// 			fNew, _ := os.Create("results/" + commit.CommitHash + "f" + sc + "_new.java")
// 			defer fNew.Close()

// 			fOld, _ := os.Create("results/" + commit.CommitHash + "f" + sc + "_old.java")
// 			defer fOld.Close()

// 			// //hunks
// 			for _, hunk := range file.Hunks {
// 				for _, l := range hunk.NewRange.Lines {
// 					fNew.WriteString(l.Content + "\n")
// 				}
// 				for _, l := range hunk.OrigRange.Lines {
// 					fOld.WriteString(l.Content + "\n")
// 				}
// 			}
// 			count++

// 		}

// 		hasher := sha1.New()
// 		patch, err := change.Patch()
// 		if err != nil {
// 			fmt.Println(err.Error())
// 		}
// 		hasher.Write([]byte(patch.String()))
// 		changeSha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))
// 		//fmt.Println(changeSha)
// 		//	id := fmt.Sprintf("%s",currCommit.ID)
// 		//	fmt.Printf("*************  %s\n", id)
// 		_, err = models.FindChangeByHash(db, changeSha, commit.ID)
// 		if err != nil {
// 			fmt.Println("new change")
// 			fmt.Println(err)
// 			action, err := change.Action()
// 			if err != nil {
// 				fmt.Println(err.Error()) //return err
// 			}
// 			changeObj := &models.Change{CommitID: commit.ID, ChangeHash: changeSha, FileFrom: change.From.Name, FileTo: change.To.Name, Action: action.String(), Patch: patch.String()}
// 			models.CreateChange(db, changeObj)

// 			//call randoop
// 			fmt.Println(change.From.Name)
// 			if action.String() == "Modify" &&
// 				strings.Contains(change.From.Name, ".java") &&
// 				strings.Contains(change.To.Name, ".java") &&
// 				!strings.HasPrefix(change.From.Name, "src/test/") &&
// 				!strings.HasPrefix(change.From.Name, "src/test/") {
// 				// CollectRandoopMetrics(repoDir, repository.Name, commit.PreviousCommitHash, change.From.Name, commit.CommitHash, change.To.Name, changeObj.ID)
// 			}
// 		} else {
// 			fmt.Println("change already exists in database...")
// 		}
// 	}
// }

func listJavaFiles(repoDir string) []string {
	var files []string
	err := filepath.Walk(repoDir, visit(&files))
	if err != nil {
		panic(err)
	}
	return files
}

func visit(files *[]string) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		if filepath.Ext(path) == ".java" {
			*files = append(*files, path)
		}

		return nil
	}
}

// exists returns whether the given file or directory exists
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func checkBuildTool(repoDir string) string {
	pomExists, err := fileExists(repoDir + "/" + "pom.xml")
	if err != nil {
		fmt.Println("ERROR looking for pom.xml...")
	}
	if pomExists {
		return "maven"
	}
	gradleExists, err := fileExists(repoDir + "/" + "settings.gradle")
	if err != nil {
		fmt.Println("ERROR looking for settings.gradle...")
	}
	if gradleExists {
		return "gradle"
	}
	return ""

}

func getProjectPaths(repoDir string) []string {
	var includes []string
	file, err := os.Open(repoDir + "/settings.gradle")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	re := regexp.MustCompile(`\(\'(.*?)\'\)`)
	for scanner.Scan() {
		str1 := scanner.Text()
		// fmt.Println(str1)
		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}

		if strings.Contains(str1, "include('") {
			submatchall := re.FindAllString(str1, -1)
			for _, element := range submatchall {
				element = strings.Trim(element, "('")
				element = strings.Trim(element, "')")
				includes = append(includes, element)
			}
		}
	}

	return includes
}
